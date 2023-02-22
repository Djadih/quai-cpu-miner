package main

import (
	"errors"
	"fmt"
	"log"
	"math/big"

	"os"
	"strconv"
	"time"

	"github.com/INFURA/go-ethlibs/jsonrpc"

	"github.com/TwiN/go-color"
	"github.com/dominant-strategies/go-quai/common"
	"github.com/dominant-strategies/go-quai/consensus/blake3pow"
	"github.com/dominant-strategies/go-quai/core/types"
	"github.com/dominant-strategies/go-quai/rpc"
	"github.com/dominant-strategies/quai-cpu-miner/util"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10
	maxRetryDelay   = 60 * 60 * 4 // 4 hours
	USER_AGENT_VER  = "0.1"
)

var (
	exit = make(chan bool)
)

type Miner struct {
	// Miner config object
	config util.Config

	// Blake3pow consensus engine used to seal a block
	engine *blake3pow.Blake3pow

	// Current header to mine
	header *types.Header

	// RPC client connections to the Quai nodes
	sliceClients SliceClients

	// Channel to receive header updates
	updateCh chan *types.Header

	// Channel to submit completed work
	resultCh chan *types.Header

	// Track previous block number for pretty printing
	previousNumber [common.HierarchyDepth]uint64
}

// Clients for RPC connection to the Prime, region, & zone node belonging to the
// slice we are actively mining
type SliceClients [common.HierarchyDepth]*rpc.MinerSession

// getNodeClients takes in a config and retrieves the Prime, Region, and Zone client
// that is used for mining in a slice.
func connectToSlice(config util.Config) SliceClients {
	var err error
	loc := config.Location
	clients := SliceClients{}
	primeConnected := false
	regionConnected := false
	zoneConnected := false
	for !primeConnected || !regionConnected || !zoneConnected {
		if config.PrimeURL != "" && !primeConnected {
			clients[common.PRIME_CTX], err = rpc.NewMinerConn(config.PrimeURL)
			if err != nil {
				log.Println("Unable to connect to node:", "Prime", config.PrimeURL)
			} else {
				primeConnected = true
			}
		}
		if config.RegionURLs[loc.Region()] != "" && !regionConnected {
			clients[common.REGION_CTX], err = rpc.NewMinerConn(config.RegionURLs[loc.Region()])
			if err != nil {
				log.Println("Unable to connect to node:", "Region", config.RegionURLs[loc.Region()])
			} else {
				regionConnected = true
			}
		}
		if config.ZoneURLs[loc.Region()][loc.Zone()] != "" && !zoneConnected {
			clients[common.ZONE_CTX], err = rpc.NewMinerConn(config.ZoneURLs[loc.Region()][loc.Zone()])
			if err != nil {
				log.Println("Unable to connect to node:", "Zone", config.ZoneURLs[loc.Region()][loc.Zone()])
			} else {
				zoneConnected = true
			}
		}
	}
	return clients
}

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func main() {
	// Load config
	config, err := util.LoadConfig("..")
	if err != nil {
		log.Fatal("cannot load config:", err)
	}
	// Parse mining location from args
	if len(os.Args) > 2 {
		raw := os.Args[1:3]
		region, _ := strconv.Atoi(raw[0])
		zone, _ := strconv.Atoi(raw[1])
		config.Location = common.Location{byte(region), byte(zone)}
	} else {
		log.Fatal("Not enough arguments supplied")
	}
	// Build manager config
	blake3Config := blake3pow.Config{
		NotifyFull: true,
	}
	blake3Engine := blake3pow.New(blake3Config, nil, false)
	m := &Miner{
		config:         config,
		engine:         blake3Engine,
		sliceClients:   connectToSlice(config),
		header:         types.EmptyHeader(),
		updateCh:       make(chan *types.Header, resultQueueSize),
		resultCh:       make(chan *types.Header, resultQueueSize),
		previousNumber: [common.HierarchyDepth]uint64{0, 0, 0},
	}
	log.Println("Starting Quai cpu miner in location ", config.Location)
	go m.startListener()
	go m.fetchPendingHeader()
	go m.subscribePendingHeader()
	go m.resultLoop()
	go m.miningLoop()
	go m.hashratePrinter()
	<-exit
}

// subscribePendingHeader subscribes to the head of the mining nodes in order to pass
// the most up to date block to the miner within the manager.
func (m *Miner) subscribePendingHeader() error {
	address := "0x4a06cbb083ce0ae76aa393db8acfced7552b7218"
	password := "password"

	msg, err := jsonrpc.MakeRequest(1, "quai_submitLogin", address, password)
	if err != nil {
		log.Fatalf("Unable to create login request: %v", err)
	}

	return m.sliceClients[common.ZONE_CTX].SendTCPRequest(*msg)
}

func (m *Miner) startListener() {
	m.sliceClients[common.ZONE_CTX].ListenTCP(m.updateCh)
}

// PendingBlocks gets the latest block when we have received a new pending header. This will get the receipts,
// transactions, and uncles to be stored during mining.
// As written, this will only run once at startup.
func (m *Miner) fetchPendingHeader() {
	retryDelay := 1 // Start retry at 1 second
	for {
		msg, err := jsonrpc.MakeRequest(1, "quai_getPendingHeader", nil)
		if err != nil {
			log.Fatalf("Unable to make pending header request: %v", err)
		}
		err = m.sliceClients[common.ZONE_CTX].SendTCPRequest(*msg)
		header := <-m.updateCh

		if err != nil {
			log.Println("Pending block not found error: ", err)
			time.Sleep(time.Duration(retryDelay) * time.Second)
			retryDelay *= 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		} else {
			m.updateCh <- header
			break
		}
	}
}

// miningLoop iterates on a new header and passes the result to m.resultCh. The result is called within the method.
func (m *Miner) miningLoop() error {
	var (
		stopCh chan struct{}
	)
	// interrupt aborts the in-flight sealing task.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}
	for {
		select {
		case header := <-m.updateCh:
			// Mine the header here
			// Return the valid header with proper nonce and mix digest
			// Interrupt previous sealing operation
			interrupt()
			stopCh = make(chan struct{})
			number := [common.HierarchyDepth]uint64{header.NumberU64(common.PRIME_CTX), header.NumberU64(common.REGION_CTX), header.NumberU64(common.ZONE_CTX)}
			primeStr := fmt.Sprint(number[common.PRIME_CTX])
			regionStr := fmt.Sprint(number[common.REGION_CTX])
			zoneStr := fmt.Sprint(number[common.ZONE_CTX])
			if number != m.previousNumber {
				if number[common.PRIME_CTX] != m.previousNumber[common.PRIME_CTX] {
					primeStr = color.Ize(color.Red, primeStr)
					regionStr = color.Ize(color.Red, regionStr)
					zoneStr = color.Ize(color.Red, zoneStr)
				} else if number[common.REGION_CTX] != m.previousNumber[common.REGION_CTX] {
					regionStr = color.Ize(color.Yellow, regionStr)
					zoneStr = color.Ize(color.Yellow, zoneStr)
				} else if number[common.ZONE_CTX] != m.previousNumber[common.ZONE_CTX] {
					zoneStr = color.Ize(color.Blue, zoneStr)
				}
				log.Println("Mining Block: ", fmt.Sprintf("[%s %s %s]", primeStr, regionStr, zoneStr), "location", header.Location(), "difficulty", header.DifficultyArray())
			}
			m.previousNumber = [common.HierarchyDepth]uint64{header.NumberU64(common.PRIME_CTX), header.NumberU64(common.REGION_CTX), header.NumberU64(common.ZONE_CTX)}
			header.SetTime(uint64(time.Now().Unix()))
			if err := m.engine.Seal(header, m.resultCh, stopCh); err != nil {
				log.Println("Block sealing failed", "err", err)
			}
		}
	}
}

// WatchHashRate is a simple method to watch the hashrate of our miner and log the output.
func (m *Miner) hashratePrinter() {
	ticker := time.NewTicker(60 * time.Second)
	toSiUnits := func(hr float64) (float64, string) {
		reduced := hr
		order := 0
		for {
			if reduced >= 1000 {
				reduced /= 1000
				order += 3
			} else {
				break
			}
		}
		switch order {
		case 3:
			return reduced, "Kh/s"
		case 6:
			return reduced, "Mh/s"
		case 9:
			return reduced, "Gh/s"
		case 12:
			return reduced, "Th/s"
		default:
			// If reduction didn't work, just return the original
			return hr, "h/s"
		}
	}
	for {
		select {
		case <-ticker.C:
			hashRate := m.engine.Hashrate()
			hr, units := toSiUnits(hashRate)
			log.Println("Current hashrate: ", hr, units)
		}
	}
}

// resultLoop takes in the result and passes to the proper channels for receiving.
func (m *Miner) resultLoop() error {
	for {
		select {
		case header := <-m.resultCh:
			// Miner only needs to be aware of the proxy (stored at zone context).
			go m.sendMinedHeader(common.ZONE_CTX, header)

			order, err := m.GetDifficultyOrder(header)
			if err != nil {
				log.Println("Block mined has an invalid order")
			}
			switch order {
			case common.PRIME_CTX:
				log.Println(color.Ize(color.Red, "PRIME block : "), header.NumberArray(), header.Hash())
			case common.REGION_CTX:
				log.Println(color.Ize(color.Yellow, "REGION block: "), header.NumberArray(), header.Hash())
			case common.ZONE_CTX:
				log.Println(color.Ize(color.Blue, "ZONE block  : "), header.NumberArray(), header.Hash())
			}
		}
	}
}

// SendMinedHeader sends the mined block to its mining client with the transactions, uncles, and receipts.
func (m *Miner) sendMinedHeader(ctx int, header *types.Header) {
	retryDelay := 1 // Start retry at 1 second
	for {
		header_msg := rpc.RPCMarshalHeader(header)
		header_req, err := jsonrpc.MakeRequest(0, "quai_receiveMinedHeader", header_msg)
		if err != nil {
			log.Fatalf("Could not send mined header: %v", err)
		}

		err = m.sliceClients[ctx].SendTCPRequest(*header_req)
		if err != nil {
			log.Printf("Unable to send pending header to node: %v", err)
			time.Sleep(time.Duration(retryDelay) * time.Second)
			retryDelay *= 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		} else {
			break
		}
		log.Println("Sent mined header")
	}
}

var (
	big2e256 = new(big.Int).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0)) // 2^256
)

// This function determines the difficulty order of a block
func (m *Miner) GetDifficultyOrder(header *types.Header) (int, error) {
	if header == nil {
		return common.HierarchyDepth, errors.New("no header provided")
	}
	blockhash := header.Hash()
	for i, difficulty := range header.DifficultyArray() {
		if difficulty != nil && big.NewInt(0).Cmp(difficulty) < 0 {
			target := new(big.Int).Div(big2e256, difficulty)
			if new(big.Int).SetBytes(blockhash.Bytes()).Cmp(target) <= 0 {
				return i, nil
			}
		}
	}
	return -1, errors.New("block does not satisfy minimum difficulty")
}
