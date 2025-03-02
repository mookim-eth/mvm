// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	mapset "github.com/deckarep/golang-set"
	"github.com/ethereum-optimism/optimism/l2geth/common"
	"github.com/ethereum-optimism/optimism/l2geth/consensus"
	"github.com/ethereum-optimism/optimism/l2geth/consensus/misc"
	"github.com/ethereum-optimism/optimism/l2geth/core"
	"github.com/ethereum-optimism/optimism/l2geth/core/state"
	"github.com/ethereum-optimism/optimism/l2geth/core/types"
	"github.com/ethereum-optimism/optimism/l2geth/event"
	"github.com/ethereum-optimism/optimism/l2geth/log"
	"github.com/ethereum-optimism/optimism/l2geth/metrics"
	"github.com/ethereum-optimism/optimism/l2geth/params"
	"github.com/ethereum-optimism/optimism/l2geth/rollup/rcfg"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10

	// chainSideChanSize is the size of channel listening to ChainSideEvent.
	chainSideChanSize = 10

	// resubmitAdjustChanSize is the size of resubmitting interval adjustment channel.
	resubmitAdjustChanSize = 10

	// miningLogAtDepth is the number of confirmations before logging successful mining.
	miningLogAtDepth = 7

	// minRecommitInterval is the minimal time interval to recreate the mining block with
	// any newly arrived transactions.
	minRecommitInterval = 1 * time.Second

	// maxRecommitInterval is the maximum time interval to recreate the mining block with
	// any newly arrived transactions.
	maxRecommitInterval = 15 * time.Second

	// intervalAdjustRatio is the impact a single interval adjustment has on sealing work
	// resubmitting interval.
	intervalAdjustRatio = 0.1

	// intervalAdjustBias is applied during the new resubmit interval calculation in favor of
	// increasing upper limit or decreasing lower limit so that the limit can be reachable.
	intervalAdjustBias = 200 * 1000.0 * 1000.0

	// staleThreshold is the maximum depth of the acceptable stale block.
	staleThreshold = 7
)

var (
	// ErrCannotCommitTxn signals that the transaction execution failed
	// when attempting to mine a transaction.
	//
	// NOTE: This error is not expected to occur in regular operation of
	// l2geth, rather the actual execution error should be returned to the
	// user.
	ErrCannotCommitTxn = errors.New("Cannot commit transaction in miner")

	// rollup apply transaction metrics
	accountReadTimer   = metrics.NewRegisteredTimer("rollup/tx/account/reads", nil)
	accountUpdateTimer = metrics.NewRegisteredTimer("rollup/tx/account/updates", nil)
	storageReadTimer   = metrics.NewRegisteredTimer("rollup/tx/storage/reads", nil)
	storageUpdateTimer = metrics.NewRegisteredTimer("rollup/tx/storage/updates", nil)
	txExecutionTimer   = metrics.NewRegisteredTimer("rollup/tx/execution", nil)
)

// environment is the worker's current environment and holds all of the current state information.
type environment struct {
	signer types.Signer

	state     *state.StateDB // apply state changes here
	ancestors mapset.Set     // ancestor set (used for checking uncle parent validity)
	family    mapset.Set     // family set (used for checking uncle invalidity)
	uncles    mapset.Set     // uncle set
	tcount    int            // tx count in cycle
	gasPool   *core.GasPool  // available gas used to pack transactions

	header   *types.Header
	txs      []*types.Transaction
	receipts []*types.Receipt
}

// task contains all information for consensus engine sealing and result submitting.
type task struct {
	receipts  []*types.Receipt
	state     *state.StateDB
	block     *types.Block
	createdAt time.Time
}

const (
	commitInterruptNone int32 = iota
	commitInterruptNewHead
	commitInterruptResubmit
)

// newWorkReq represents a request for new sealing work submitting with relative interrupt notifier.
type newWorkReq struct {
	interrupt *int32
	timestamp int64
}

// intervalAdjust represents a resubmitting interval adjustment.
type intervalAdjust struct {
	ratio float64
	inc   bool
}

// worker is the main object which takes care of submitting new work to consensus engine
// and gathering the sealing result.
type worker struct {
	config      *Config
	chainConfig *params.ChainConfig
	engine      consensus.Engine
	eth         Backend
	chain       *core.BlockChain

	// Feeds
	pendingLogsFeed event.Feed

	// Subscriptions
	mux              *event.TypeMux
	txsCh            chan core.NewTxsEvent
	txsSub           event.Subscription
	chainHeadCh      chan core.ChainHeadEvent
	chainHeadSub     event.Subscription
	chainSideCh      chan core.ChainSideEvent
	chainSideSub     event.Subscription
	rollupCh         chan core.NewTxsEvent
	rollupSub        event.Subscription
	rollupSubOtherTx event.Subscription
	rollupOtherTxCh  chan core.NewTxsEvent

	// Channels
	newWorkCh          chan *newWorkReq
	taskCh             chan *task
	resultCh           chan *types.Block
	startCh            chan struct{}
	exitCh             chan struct{}
	resubmitIntervalCh chan time.Duration
	resubmitAdjustCh   chan *intervalAdjust

	current      *environment                 // An environment for current running cycle.
	localUncles  map[common.Hash]*types.Block // A set of side blocks generated locally as the possible uncle blocks.
	remoteUncles map[common.Hash]*types.Block // A set of side blocks as the possible uncle blocks.
	unconfirmed  *unconfirmedBlocks           // A set of locally mined blocks pending canonicalness confirmations.

	mu       sync.RWMutex // The lock used to protect the coinbase and extra fields
	coinbase common.Address
	extra    []byte

	pendingMu    sync.RWMutex
	pendingTasks map[common.Hash]*task

	snapshotMu    sync.RWMutex // The lock used to protect the block snapshot and state snapshot
	snapshotBlock *types.Block
	snapshotState *state.StateDB

	// atomic status counters
	running int32 // The indicator whether the consensus engine is running or not.
	newTxs  int32 // New arrival transaction count since last sealing work submitting.

	// External functions
	isLocalBlock func(block *types.Block) bool // Function used to determine whether the specified block is mined by local miner.

	// Test hooks
	newTaskHook  func(*task)                        // Method to call upon receiving a new sealing task.
	skipSealHook func(*task) bool                   // Method to decide whether skipping the sealing.
	fullTaskHook func()                             // Method to call before pushing the full sealing task.
	resubmitHook func(time.Duration, time.Duration) // Method to call upon updating resubmitting interval.
}

func newWorker(config *Config, chainConfig *params.ChainConfig, engine consensus.Engine, eth Backend, mux *event.TypeMux, isLocalBlock func(*types.Block) bool, init bool) *worker {
	worker := &worker{
		config:             config,
		chainConfig:        chainConfig,
		engine:             engine,
		eth:                eth,
		mux:                mux,
		chain:              eth.BlockChain(),
		isLocalBlock:       isLocalBlock,
		localUncles:        make(map[common.Hash]*types.Block),
		remoteUncles:       make(map[common.Hash]*types.Block),
		unconfirmed:        newUnconfirmedBlocks(eth.BlockChain(), miningLogAtDepth),
		pendingTasks:       make(map[common.Hash]*task),
		txsCh:              make(chan core.NewTxsEvent, txChanSize),
		rollupCh:           make(chan core.NewTxsEvent, 1),
		chainHeadCh:        make(chan core.ChainHeadEvent, chainHeadChanSize),
		chainSideCh:        make(chan core.ChainSideEvent, chainSideChanSize),
		newWorkCh:          make(chan *newWorkReq),
		taskCh:             make(chan *task),
		resultCh:           make(chan *types.Block, resultQueueSize),
		exitCh:             make(chan struct{}),
		startCh:            make(chan struct{}, 1),
		resubmitIntervalCh: make(chan time.Duration),
		resubmitAdjustCh:   make(chan *intervalAdjust, resubmitAdjustChanSize),

		rollupOtherTxCh: make(chan core.NewTxsEvent, 1),
	}
	// Subscribe NewTxsEvent for tx pool
	worker.txsSub = eth.TxPool().SubscribeNewTxsEvent(worker.txsCh)
	// channel directly to the miner
	worker.rollupSub = eth.SyncService().SubscribeNewTxsEvent(worker.rollupCh)

	// chanel directly to the miner for syncing from other
	worker.rollupSubOtherTx = eth.SyncService().SubscribeNewOtherTxsEvent(worker.rollupOtherTxCh)

	// Subscribe events for blockchain
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)
	worker.chainSideSub = eth.BlockChain().SubscribeChainSideEvent(worker.chainSideCh)

	// Sanitize recommit interval if the user-specified one is too short.
	recommit := worker.config.Recommit
	if recommit < minRecommitInterval {
		log.Warn("Sanitizing miner recommit interval", "provided", recommit, "updated", minRecommitInterval)
		recommit = minRecommitInterval
	}

	go worker.mainLoop()
	go worker.newWorkLoop(recommit)
	go worker.resultLoop()
	go worker.taskLoop()

	// Submit first work to initialize pending state.
	if init {
		worker.startCh <- struct{}{}
	}
	return worker
}

// setEtherbase sets the etherbase used to initialize the block coinbase field.
func (w *worker) setEtherbase(addr common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.coinbase = addr
}

// setExtra sets the content used to initialize the block extra field.
func (w *worker) setExtra(extra []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = extra
}

// setRecommitInterval updates the interval for miner sealing work recommitting.
func (w *worker) setRecommitInterval(interval time.Duration) {
	w.resubmitIntervalCh <- interval
}

// pending returns the pending state and corresponding block.
func (w *worker) pending() (*types.Block, *state.StateDB) {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	if w.snapshotState == nil {
		return nil, nil
	}
	return w.snapshotBlock, w.snapshotState.Copy()
}

// pendingBlock returns pending block.
func (w *worker) pendingBlock() *types.Block {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock
}

// start sets the running status as 1 and triggers new work submitting.
func (w *worker) start() {
	atomic.StoreInt32(&w.running, 1)
	w.startCh <- struct{}{}
}

// stop sets the running status as 0.
func (w *worker) stop() {
	atomic.StoreInt32(&w.running, 0)
}

// isRunning returns an indicator whether worker is running or not.
func (w *worker) isRunning() bool {
	return atomic.LoadInt32(&w.running) == 1
}

// close terminates all background threads maintained by the worker.
// Note the worker does not support being closed multiple times.
func (w *worker) close() {
	close(w.exitCh)
}

// newWorkLoop is a standalone goroutine to submit new mining work upon received events.
func (w *worker) newWorkLoop(recommit time.Duration) {
	var (
		interrupt   *int32
		minRecommit = recommit // minimal resubmit interval specified by user.
		timestamp   int64      // timestamp for each round of mining.
	)

	timer := time.NewTimer(0)
	<-timer.C // discard the initial tick

	// commit aborts in-flight transaction execution with given signal and resubmits a new one.
	commit := func(s int32) {
		if interrupt != nil {
			atomic.StoreInt32(interrupt, s)
		}
		interrupt = new(int32)
		w.newWorkCh <- &newWorkReq{interrupt: interrupt, timestamp: timestamp}
		timer.Reset(recommit)
		atomic.StoreInt32(&w.newTxs, 0)
	}
	// recalcRecommit recalculates the resubmitting interval upon feedback.
	recalcRecommit := func(target float64, inc bool) {
		var (
			prev = float64(recommit.Nanoseconds())
			next float64
		)
		if inc {
			next = prev*(1-intervalAdjustRatio) + intervalAdjustRatio*(target+intervalAdjustBias)
			// Recap if interval is larger than the maximum time interval
			if next > float64(maxRecommitInterval.Nanoseconds()) {
				next = float64(maxRecommitInterval.Nanoseconds())
			}
		} else {
			next = prev*(1-intervalAdjustRatio) + intervalAdjustRatio*(target-intervalAdjustBias)
			// Recap if interval is less than the user specified minimum
			if next < float64(minRecommit.Nanoseconds()) {
				next = float64(minRecommit.Nanoseconds())
			}
		}
		recommit = time.Duration(int64(next))
	}
	// clearPending cleans the stale pending tasks.
	clearPending := func(number uint64) {
		w.pendingMu.Lock()
		for h, t := range w.pendingTasks {
			if t.block.NumberU64()+staleThreshold <= number {
				delete(w.pendingTasks, h)
			}
		}
		w.pendingMu.Unlock()
	}

	for {
		select {
		case <-w.startCh:
			clearPending(w.chain.CurrentBlock().NumberU64())
			commit(commitInterruptNewHead)

		// TODO check this should be removed
		// Remove this code for the OVM implementation. It is responsible for
		// cleaning up memory with the call to `clearPending`, so be sure to
		// call that in the new hot code path
		/*
			case head := <-w.chainHeadCh:
				clearPending(head.Block.NumberU64())
				timestamp = time.Now().Unix()
				commit(commitInterruptNewHead)
		*/

		case <-timer.C:
			resubmit := w.chainConfig.Clique == nil || w.chainConfig.Clique.Period > 0
			nextBN := w.chain.CurrentBlock().NumberU64() + 1
			if rcfg.DeSeqBlock > 0 && nextBN >= rcfg.DeSeqBlock {
				resubmit = true
			}
			if !resubmit {
				// always enable timer
				timer.Reset(recommit)
			}
			// log.Debug("Special info in worker: newWorkLoop timer.C", "resubmit", resubmit, "cmp to DeSeqBlock", w.chain.CurrentBlock().NumberU64()+1)
			seqModel, mpcEnabled := w.eth.SyncService().GetSeqAndMpcStatus()
			if !seqModel {
				continue
			}
			// If mining is running resubmit a new work cycle periodically to pull in
			// higher priced transactions. Disable this overhead for pending blocks.
			if w.isRunning() && resubmit {
				// Short circuit if no new transaction arrives.
				if atomic.LoadInt32(&w.newTxs) == 0 {
					timer.Reset(recommit)
					continue
				}
				if mpcEnabled {
					// if is current sequencer or not
					expectSeq, err := w.eth.SyncService().GetTxSequencer(nil, nextBN)
					if err != nil {
						log.Warn("GetTxSequencer error in worker timer", "err", err)
						continue
					}
					if !w.eth.SyncService().IsSelfSeqAddress(expectSeq) {
						log.Warn("Current sequencer incorrect in worker timer, clear pending", "current sequencer", expectSeq.String())
						clearPending(w.chain.CurrentBlock().NumberU64())
						continue
					}
					// check startup sync height of L2 RPC
					if !w.eth.SyncService().IsAboveStartHeight(nextBN) {
						log.Warn("Block number below sync start height in worker timer, clear pending", "block number", nextBN)
						clearPending(w.chain.CurrentBlock().NumberU64())
						continue
					}
				}
				timestamp = time.Now().Unix()
				commit(commitInterruptResubmit)
			}

		case interval := <-w.resubmitIntervalCh:
			log.Debug("Special info in worker: resubmitIntervalCh", "minRecommitInterval", minRecommitInterval, "interval", interval)
			// Adjust resubmit interval explicitly by user.
			if interval < minRecommitInterval {
				log.Warn("Sanitizing miner recommit interval", "provided", interval, "updated", minRecommitInterval)
				interval = minRecommitInterval
			}
			log.Info("Miner recommit interval update", "from", minRecommit, "to", interval)
			minRecommit, recommit = interval, interval

			if w.resubmitHook != nil {
				log.Debug("Special info in worker: resubmitIntervalCh hook")
				w.resubmitHook(minRecommit, recommit)
			}

		case adjust := <-w.resubmitAdjustCh:
			// Adjust resubmit interval by feedback.
			if adjust.inc {
				before := recommit
				recalcRecommit(float64(recommit.Nanoseconds())/adjust.ratio, true)
				log.Trace("Increase miner recommit interval", "from", before, "to", recommit)
			} else {
				before := recommit
				recalcRecommit(float64(minRecommit.Nanoseconds()), false)
				log.Trace("Decrease miner recommit interval", "from", before, "to", recommit)
			}

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case <-w.exitCh:
			return
		}
	}
}

// mainLoop is a standalone goroutine to regenerate the sealing task based on the received event.
func (w *worker) mainLoop() {
	defer w.txsSub.Unsubscribe()
	defer w.chainHeadSub.Unsubscribe()
	defer w.chainSideSub.Unsubscribe()
	defer w.rollupSub.Unsubscribe()
	defer w.rollupSubOtherTx.Unsubscribe()
	for {
		select {
		case req := <-w.newWorkCh:
			w.commitNewWork(req.interrupt, req.timestamp)

		case ev := <-w.chainSideCh:
			// Short circuit for duplicate side blocks
			if _, exist := w.localUncles[ev.Block.Hash()]; exist {
				continue
			}
			if _, exist := w.remoteUncles[ev.Block.Hash()]; exist {
				continue
			}
			// Add side block to possible uncle block set depending on the author.
			if w.isLocalBlock != nil && w.isLocalBlock(ev.Block) {
				w.localUncles[ev.Block.Hash()] = ev.Block
			} else {
				w.remoteUncles[ev.Block.Hash()] = ev.Block
			}
			// If our mining block contains less than 2 uncle blocks,
			// add the new uncle block if valid and regenerate a mining block.
			if w.isRunning() && w.current != nil && w.current.uncles.Cardinality() < 2 {
				start := time.Now()
				if err := w.commitUncle(w.current, ev.Block.Header()); err == nil {
					var uncles []*types.Header
					w.current.uncles.Each(func(item interface{}) bool {
						hash, ok := item.(common.Hash)
						if !ok {
							return false
						}
						uncle, exist := w.localUncles[hash]
						if !exist {
							uncle, exist = w.remoteUncles[hash]
						}
						if !exist {
							return false
						}
						uncles = append(uncles, uncle.Header())
						return false
					})
					w.commit(uncles, nil, start)
				}
			}
		case ev := <-w.rollupOtherTxCh:
			// Read chainHeadCh which pushed by p2p block inserted
			if len(ev.Txs) == 0 {
				log.Warn("No transaction sent to miner from syncservice rollupOtherTxCh")
			}
			if len(w.chainHeadCh) > 0 {
				head := <-w.chainHeadCh
				txs := head.Block.Transactions()
				if len(txs) == 0 {
					log.Warn("No transactions in block rollupOtherTxCh")
					continue
				}
				txn := txs[0]
				height := head.Block.Number().Uint64()
				log.Debug("Miner got new head rollupOtherTxCh", "height", height, "block-hash", head.Block.Hash().Hex(), "tx-hash", txn.Hash().Hex())
			}
		// Read from the sync service and mine single txs
		// as they come. Wait for the block to be mined before
		// reading the next tx from the channel when there is
		// not an error processing the transaction.
		case ev := <-w.rollupCh:
			if len(ev.Txs) == 0 {
				log.Warn("No transaction sent to miner from syncservice")
				if ev.ErrCh != nil {
					ev.ErrCh <- errors.New("no transaction sent to miner from syncservice")
				} else {
					w.handleErrInTask(errors.New("no transaction sent to miner from syncservice"), false)
				}
				continue
			}
			var err error
			if rcfg.DeSeqBlock > 0 && w.chain.CurrentBlock().NumberU64()+1 >= rcfg.DeSeqBlock && (ev.Time > 0 || len(ev.Txs) > 1) {
				log.Debug("Attempting to commit rollup transactions", "hash0", ev.Txs[0].Hash().Hex())
				err = w.commitNewTxDeSeq(ev.Txs, ev.Time)
			} else {
				tx := ev.Txs[0]
				log.Debug("Attempting to commit rollup transaction", "hash", tx.Hash().Hex())
				err = w.commitNewTx(tx)
			}
			// Build the block with the tx and add it to the chain. This will
			// send the block through the `taskCh` and then through the
			// `resultCh` which ultimately adds the block to the blockchain
			// through `bc.WriteBlockWithState`
			if err == nil {
				log.Debug("Success committing transaction", "tx-hash", ev.Txs[0].Hash().Hex())
			} else {
				log.Error("Problem committing transaction", "msg", err)
				if ev.ErrCh != nil {
					ev.ErrCh <- err
				} else {
					w.handleErrInTask(err, false)
				}
			}

		case ev := <-w.txsCh:
			// Apply transactions to the pending state if we're not mining.
			//
			// Note all transactions received may not be continuous with transactions
			// already included in the current mining block. These transactions will
			// be automatically eliminated.
			if !w.isRunning() && w.current != nil {
				// If block is already full, abort
				if gp := w.current.gasPool; gp != nil && gp.Gas() < params.TxGas {
					log.Debug("Special info in worker", "isfull", true)
					continue
				}
				log.Debug("Special info in worker", "isfull", false)
				w.mu.RLock()
				coinbase := w.coinbase
				w.mu.RUnlock()

				txs := make(map[common.Address]types.Transactions)
				for _, tx := range ev.Txs {
					acc, _ := types.Sender(w.current.signer, tx)
					txs[acc] = append(txs[acc], tx)
				}
				txset := types.NewTransactionsByPriceAndNonce(w.current.signer, txs)
				tcount := w.current.tcount
				w.commitTransactions(txset, coinbase, nil)
				// Only update the snapshot if any new transactons were added
				// to the pending block
				if tcount != w.current.tcount {
					w.updateSnapshot()
				}
			} else {
				// If clique is running in dev mode(period is 0), disable
				// advance sealing here.
				deSeqModel := false
				if rcfg.DeSeqBlock > 0 && w.chain.CurrentBlock().NumberU64()+1 >= rcfg.DeSeqBlock {
					deSeqModel = true
				}
				log.Debug("Special info in worker", "working else", true, "deSeqMode", deSeqModel, "cmp to DeSeqBlock", w.chain.CurrentBlock().NumberU64()+1)
				if w.chainConfig.Clique != nil && w.chainConfig.Clique.Period == 0 && !deSeqModel {
					w.commitNewWork(nil, time.Now().Unix())
				}
			}
			atomic.AddInt32(&w.newTxs, int32(len(ev.Txs)))

		// System stopped
		case <-w.exitCh:
			return
		case <-w.txsSub.Err():
			return
		case <-w.chainHeadSub.Err():
			return
		case <-w.chainSideSub.Err():
			return
		}
	}
}

// taskLoop is a standalone goroutine to fetch sealing task from the generator and
// push them to consensus engine.
func (w *worker) taskLoop() {
	var (
		stopCh chan struct{}
		prev   common.Hash
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
		case task := <-w.taskCh:
			if w.newTaskHook != nil {
				w.newTaskHook(task)
			}
			// Reject duplicate sealing work due to resubmitting.
			sealHash := w.engine.SealHash(task.block.Header())
			if sealHash == prev {
				w.handleErrInTask(errors.New("Block sealing ignored"), true)
				log.Error("Block sealing ignored", "sealHash", sealHash)
				continue
			}
			// Interrupt previous sealing operation
			interrupt()
			stopCh, prev = make(chan struct{}), sealHash

			if w.skipSealHook != nil && w.skipSealHook(task) {
				w.handleErrInTask(errors.New("Block sealing skiped"), true)
				log.Error("Block sealing skiped", "sealHash", sealHash)
				continue
			}
			w.pendingMu.Lock()
			w.pendingTasks[w.engine.SealHash(task.block.Header())] = task
			w.pendingMu.Unlock()

			if err := w.engine.Seal(w.chain, task.block, w.resultCh, stopCh); err != nil {
				w.handleErrInTask(err, true)
				log.Error("Block sealing failed", "err", err)
			}
		case <-w.exitCh:
			interrupt()
			return
		}
	}
}

// resultLoop is a standalone goroutine to handle sealing result submitting
// and flush relative data to the database.
func (w *worker) resultLoop() {
	for {
		select {
		case block := <-w.resultCh:
			// Short circuit when receiving empty result.
			if block == nil {
				w.handleErrInTask(errors.New("Block is nil"), true)
				continue
			}
			// Short circuit when receiving duplicate result caused by resubmitting.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				w.handleErrInTask(errors.New("Dulplicate block"), true)
				continue
			}
			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)
			w.pendingMu.RLock()
			task, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()
			if !exist {
				log.Error("Block found but no relative pending task", "number", block.Number(), "sealhash", sealhash, "hash", hash)
				w.handleErrInTask(errors.New("Block found but no relative pending task"), true)
				continue
			}
			// Different block could share same sealhash, deep copy here to prevent write-write conflict.
			var (
				receipts = make([]*types.Receipt, len(task.receipts))
				logs     []*types.Log
			)
			for i, receipt := range task.receipts {
				// add block location fields
				receipt.BlockHash = hash
				receipt.BlockNumber = block.Number()
				receipt.TransactionIndex = uint(i)

				receipts[i] = new(types.Receipt)
				*receipts[i] = *receipt
				// Update the block hash in all logs since it is now available and not when the
				// receipt/log of individual transactions were created.
				for _, log := range receipt.Logs {
					log.BlockHash = hash
				}
				logs = append(logs, receipt.Logs...)
			}
			// Commit block and state to database.
			_, err := w.chain.WriteBlockWithState(block, receipts, logs, task.state, true)
			if err != nil {
				w.handleErrInTask(err, true)
				log.Error("Failed writing block to chain", "err", err)
				continue
			}

			// Move this from rollupCh select to make sure deSeqModel can read chainHeadCh too

			// `chainHeadCh` is written to when a new block is added to the
			// tip of the chain. Reading from the channel will block until
			// the ethereum block is added to the chain downstream of `commitNewTx`.
			// This will result in a deadlock if we call `commitNewTx` with
			// a transaction that cannot be added to the chain, so this
			// should be updated to a select statement that can also listen
			// for errors.
			head := <-w.chainHeadCh
			txs := head.Block.Transactions()
			if len(txs) == 0 {
				log.Warn("No transactions in block")
				continue
			}
			txn := txs[0]
			height := head.Block.Number().Uint64()
			log.Debug("Miner got new head", "height", height, "block-hash", head.Block.Hash().Hex(), "txn-hash", txn.Hash().Hex())

			// Prevent memory leak by cleaning up pending tasks
			// This is mostly copied from the `newWorkLoop`
			// `clearPending` function and must be called
			// periodically to clean up pending tasks. This
			// function was originally called in `newWorkLoop`
			// but the OVM implementation no longer uses that code path.
			w.pendingMu.Lock()
			for h := range w.pendingTasks {
				delete(w.pendingTasks, h)
			}
			w.pendingMu.Unlock()

			// Broadcast the block and announce chain insertion event
			w.mux.Post(core.NewMinedBlockEvent{Block: block})

			// Insert the block into the set of pending ones to resultLoop for confirmations
			w.unconfirmed.Insert(block.NumberU64(), block.Hash())

		case <-w.exitCh:
			return
		}
	}
}

// makeCurrent creates a new environment for the current cycle.
func (w *worker) makeCurrent(parent *types.Block, header *types.Header) error {
	state, err := w.chain.StateAt(parent.Root())
	if err != nil {
		return err
	}
	env := &environment{
		signer:    types.NewEIP155Signer(w.chainConfig.ChainID),
		state:     state,
		ancestors: mapset.NewSet(),
		family:    mapset.NewSet(),
		uncles:    mapset.NewSet(),
		header:    header,
	}

	// when 08 is processed ancestors contain 07 (quick block)
	for _, ancestor := range w.chain.GetBlocksFromHash(parent.Hash(), 7) {
		for _, uncle := range ancestor.Uncles() {
			env.family.Add(uncle.Hash())
		}
		env.family.Add(ancestor.Hash())
		env.ancestors.Add(ancestor.Hash())
	}

	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0
	w.current = env
	return nil
}

// commitUncle adds the given block to uncle block set, returns error if failed to add.
func (w *worker) commitUncle(env *environment, uncle *types.Header) error {
	hash := uncle.Hash()
	if env.uncles.Contains(hash) {
		return errors.New("uncle not unique")
	}
	if env.header.ParentHash == uncle.ParentHash {
		return errors.New("uncle is sibling")
	}
	if !env.ancestors.Contains(uncle.ParentHash) {
		return errors.New("uncle's parent unknown")
	}
	if env.family.Contains(hash) {
		return errors.New("uncle already included")
	}
	env.uncles.Add(uncle.Hash())
	return nil
}

// updateSnapshot updates pending snapshot block and state.
// Note this function assumes the current variable is thread safe.
func (w *worker) updateSnapshot() {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()

	var uncles []*types.Header
	w.current.uncles.Each(func(item interface{}) bool {
		hash, ok := item.(common.Hash)
		if !ok {
			return false
		}
		uncle, exist := w.localUncles[hash]
		if !exist {
			uncle, exist = w.remoteUncles[hash]
		}
		if !exist {
			return false
		}
		uncles = append(uncles, uncle.Header())
		return false
	})

	w.snapshotBlock = types.NewBlock(
		w.current.header,
		w.current.txs,
		uncles,
		w.current.receipts,
	)

	w.snapshotState = w.current.state.Copy()
}

func (w *worker) commitTransaction(tx *types.Transaction, coinbase common.Address) ([]*types.Log, error) {
	// Make sure there's only one tx per block
	if w.current != nil && len(w.current.txs) > 0 {
		// after DeSeqBlock, allow multiple tx in a pool, header number with new block
		log.Debug("Special info in worker: commitTransaction", "cmp to DeSeqBlock", w.current.header.Number.Uint64())
		if rcfg.UsingOVM && !(rcfg.DeSeqBlock > 0 && w.current.header.Number.Uint64() >= rcfg.DeSeqBlock) {
			return nil, core.ErrGasLimitReached
		}
	}
	snap := w.current.state.Snapshot()

	start := time.Now()
	receipt, err := core.ApplyTransaction(w.chainConfig, w.chain, &coinbase, w.current.gasPool, w.current.state, w.current.header, tx, &w.current.header.GasUsed, *w.chain.GetVMConfig())
	if err != nil {
		w.current.state.RevertToSnapshot(snap)
		return nil, err
	}
	w.current.txs = append(w.current.txs, tx)
	w.current.receipts = append(w.current.receipts, receipt)

	updateTransactionStateMetrics(start, w.current.state)

	return receipt.Logs, nil
}

func (w *worker) commitTransactions(txs *types.TransactionsByPriceAndNonce, coinbase common.Address, interrupt *int32) bool {
	return w.commitTransactionsWithError(txs, coinbase, interrupt) != nil
}

func (w *worker) commitTransactionsWithError(txs *types.TransactionsByPriceAndNonce, coinbase common.Address, interrupt *int32) error {
	// Short circuit if current is nil
	if w.current == nil {
		return ErrCannotCommitTxn
	}

	if w.current.gasPool == nil {
		w.current.gasPool = new(core.GasPool).AddGas(w.current.header.GasLimit)
	}

	var coalescedLogs []*types.Log

	parent := w.chain.CurrentBlock()
	pn := parent.Number().Uint64()
	l1TS := uint64(0)
	l1BN := uint64(0)
	deSeqModel := false
	log.Debug("Special info in worker: commitTransactionsWithError", "cmp to DeSeqBlock", pn+1)
	if rcfg.DeSeqBlock > 0 && pn+1 >= rcfg.DeSeqBlock {
		deSeqModel = true
	}

	for {
		// In the following three cases, we will interrupt the execution of the transaction.
		// (1) new head block event arrival, the interrupt signal is 1
		// (2) worker start or restart, the interrupt signal is 1
		// (3) worker recreate the mining block with any newly arrived transactions, the interrupt signal is 2.
		// For the first two cases, the semi-finished work will be discarded.
		// For the third case, the semi-finished work will be submitted to the consensus engine.
		if interrupt != nil && atomic.LoadInt32(interrupt) != commitInterruptNone {
			// Notify resubmit loop to increase resubmitting interval due to too frequent commits.
			if atomic.LoadInt32(interrupt) == commitInterruptResubmit {
				ratio := float64(w.current.header.GasLimit-w.current.gasPool.Gas()) / float64(w.current.header.GasLimit)
				if ratio < 0.1 {
					ratio = 0.1
				}
				w.resubmitAdjustCh <- &intervalAdjust{
					ratio: ratio,
					inc:   true,
				}
			}
			if w.current.tcount == 0 ||
				atomic.LoadInt32(interrupt) == commitInterruptNewHead {
				return ErrCannotCommitTxn
			}
			return nil
		}
		// If we don't have enough gas for any further transactions then we're done
		if w.current.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", w.current.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}

		// setIndex, l1Timestamp, l1BlockNumber
		if deSeqModel {
			tx.SetIndex(pn)
			if l1BN == 0 {
				l1BN = tx.L1BlockNumber().Uint64()
				l1TS = tx.L1Timestamp()
			} else {
				tx.SetL1BlockNumber(l1BN)
				tx.SetL1Timestamp(l1TS)
			}
		}

		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(w.current.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !w.chainConfig.IsEIP155(w.current.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", w.chainConfig.EIP155Block)

			txs.Pop()
			continue
		}
		// Start executing the transaction
		w.current.state.Prepare(tx.Hash(), common.Hash{}, w.current.tcount)

		logs, err := w.commitTransaction(tx, coinbase)
		switch err {
		case core.ErrGasLimitReached:
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Trace("Gas limit exceeded for current block", "sender", from)
			txs.Pop()

		case core.ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			txs.Shift()

		case core.ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Trace("Skipping account with high nonce", "sender", from, "nonce", tx.Nonce())
			txs.Pop()

		case nil:
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			w.current.tcount++
			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			if rcfg.UsingOVM {
				txs.Pop()
			} else {
				txs.Shift()
			}
		}
		// UsingOVM
		// Return specific execution errors directly to the user to
		// avoid returning the generic ErrCannotCommitTxnErr. It is safe
		// to return the error directly since l2geth only processes at
		// most one transaction per block.
		if err != nil {
			return err
		}
	}

	if !w.isRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are mining. The reason is that
		// when we are mining, the worker will regenerate a mining block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.

		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		w.pendingLogsFeed.Send(cpy)
	}
	// Notify resubmit loop to decrease resubmitting interval if current interval is larger
	// than the user-specified one.
	if interrupt != nil {
		w.resubmitAdjustCh <- &intervalAdjust{inc: false}
	}
	if w.current.tcount == 0 {
		return ErrCannotCommitTxn
	}
	return nil
}

// commitNewTx is an OVM addition that mines a block with a single tx in it.
// It needs to return an error in the case there is an error to prevent waiting
// on reading from a channel that is written to when a new block is added to the
// chain.
func (w *worker) commitNewTx(tx *types.Transaction) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	tstart := time.Now()

	parent := w.chain.CurrentBlock()
	num := parent.Number()

	// Preserve liveliness as best as possible. Must panic on L1 to L2
	// transactions as the timestamp cannot be malleated
	if parent.Time() > tx.L1Timestamp() {
		log.Error("Monotonicity violation", "index", num, "parent", parent.Time(), "tx", tx.L1Timestamp())
	}
	if meta := tx.GetMeta(); meta.Index != nil {
		index := num.Uint64()
		if *meta.Index < index {
			log.Info("commitNewTx ", "get meta index ", *meta.Index, "parent.Number() ", index)
			return fmt.Errorf("Failed to check meta index too small: %d, parent number: %d", *meta.Index, index)
		}
		// Check meta.Index again, it should be equal with index
		if *meta.Index > index {
			return fmt.Errorf("Failed to check meta index too large: %d, parent number: %d", *meta.Index, index)
		}
	}
	// Fill in the index field in the tx meta if it is `nil`.
	// This should only ever happen in the case of the sequencer
	// receiving a queue origin sequencer transaction. The verifier
	// should always receive transactions with an index as they
	// have already been confirmed in the canonical transaction chain.
	// Use the parent's block number because the CTC is 0 indexed.
	if meta := tx.GetMeta(); meta.Index == nil {
		index := num.Uint64()
		meta.Index = &index
		tx.SetTransactionMeta(meta)
	}
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(num, common.Big1),
		GasLimit:   w.config.GasFloor,
		Extra:      w.extra,
		Time:       tx.L1Timestamp(),
	}
	if err := w.engine.Prepare(w.chain, header); err != nil {
		return fmt.Errorf("Failed to prepare header for mining: %w", err)
	}
	// Could potentially happen if starting to mine in an odd state.
	err := w.makeCurrent(parent, header)
	if err != nil {
		return fmt.Errorf("Failed to create mining context: %w", err)
	}
	transactions := make(map[common.Address]types.Transactions)
	acc, _ := types.Sender(w.current.signer, tx)
	transactions[acc] = types.Transactions{tx}
	txs := types.NewTransactionsByPriceAndNonce(w.current.signer, transactions)
	if err := w.commitTransactionsWithError(txs, w.coinbase, nil); err != nil {
		return err
	}
	return w.commit(nil, w.fullTaskHook, tstart)
}

func (w *worker) commitNewTxDeSeq(txs []*types.Transaction, blockTime uint64) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	tstart := time.Now()

	parent := w.chain.CurrentBlock()
	num := parent.Number()

	// Preserve liveliness as best as possible. Must panic on L1 to L2
	// transactions as the timestamp cannot be malleated
	if parent.Time() > blockTime {
		log.Error("Monotonicity violation", "index", num, "parent", parent.Time(), "block", blockTime)
	}
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(num, common.Big1),
		GasLimit:   w.config.GasFloor,
		Extra:      w.extra,
		Time:       blockTime,
	}
	if err := w.engine.Prepare(w.chain, header); err != nil {
		return fmt.Errorf("Failed to prepare header for mining: %w", err)
	}
	// Could potentially happen if starting to mine in an odd state.
	err := w.makeCurrent(parent, header)
	if err != nil {
		return fmt.Errorf("Failed to create mining context: %w", err)
	}

	for _, tx := range txs {
		transacions := make(map[common.Address]types.Transactions)
		if meta := tx.GetMeta(); meta.Index != nil {
			index := num.Uint64()
			if *meta.Index < index {
				log.Info("commitNewTx ", "get meta index ", *meta.Index, "parent.Number() ", index)
				return fmt.Errorf("Failed to check meta index too small: %w, parent number: %w", *meta.Index, index)
			}
			// Check meta.Index again, it should be equal with index
			if *meta.Index > index {
				return fmt.Errorf("Failed to check meta index too large: %w, parent number: %w", *meta.Index, index)
			}
		}
		if meta := tx.GetMeta(); meta.Index == nil {
			index := num.Uint64()
			meta.Index = &index
			tx.SetTransactionMeta(meta)
		}
		acc, _ := types.Sender(w.current.signer, tx)
		transacions[acc] = append(transacions[acc], tx)
		txset := types.NewTransactionsByPriceAndNonce(w.current.signer, transacions)
		if err := w.commitTransactionsWithError(txset, w.coinbase, nil); err != nil {
			return err
		}
	}
	return w.commit(nil, w.fullTaskHook, tstart)
}

// commitNewWork generates several new sealing tasks based on the parent block.
func (w *worker) commitNewWork(interrupt *int32, timestamp int64) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	log.Debug("Special info in worker: commitNewWork", "timestamp", timestamp)

	tstart := time.Now()
	parent := w.chain.CurrentBlock()

	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   w.config.GasFloor,
		Extra:      w.extra,
		Time:       uint64(timestamp),
	}
	// Only set the coinbase if our consensus engine is running (avoid spurious block rewards)
	if w.isRunning() {
		if w.coinbase == (common.Address{}) {
			log.Error("Refusing to mine without etherbase")
			return
		}
		header.Coinbase = w.coinbase
	}
	if err := w.engine.Prepare(w.chain, header); err != nil {
		log.Error("Failed to prepare header for mining", "err", err)
		return
	}
	// If we are care about TheDAO hard-fork check whether to override the extra-data or not
	if daoBlock := w.chainConfig.DAOForkBlock; daoBlock != nil {
		// Check whether the block is among the fork extra-override range
		limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange)
		if header.Number.Cmp(daoBlock) >= 0 && header.Number.Cmp(limit) < 0 {
			// Depending whether we support or oppose the fork, override differently
			if w.chainConfig.DAOForkSupport {
				header.Extra = common.CopyBytes(params.DAOForkBlockExtra)
			} else if bytes.Equal(header.Extra, params.DAOForkBlockExtra) {
				header.Extra = []byte{} // If miner opposes, don't let it use the reserved extra-data
			}
		}
	}
	// Could potentially happen if starting to mine in an odd state.
	err := w.makeCurrent(parent, header)
	if err != nil {
		log.Error("Failed to create mining context", "err", err)
		return
	}
	// Create the current work task and check any fork transitions needed
	env := w.current
	if w.chainConfig.DAOForkSupport && w.chainConfig.DAOForkBlock != nil && w.chainConfig.DAOForkBlock.Cmp(header.Number) == 0 {
		misc.ApplyDAOHardFork(env.state)
	}
	// Accumulate the uncles for the current block
	uncles := make([]*types.Header, 0, 2)
	commitUncles := func(blocks map[common.Hash]*types.Block) {
		// Clean up stale uncle blocks first
		for hash, uncle := range blocks {
			if uncle.NumberU64()+staleThreshold <= header.Number.Uint64() {
				delete(blocks, hash)
			}
		}
		for hash, uncle := range blocks {
			if len(uncles) == 2 {
				break
			}
			if err := w.commitUncle(env, uncle.Header()); err != nil {
				log.Trace("Possible uncle rejected", "hash", hash, "reason", err)
			} else {
				log.Debug("Committing new uncle to block", "hash", hash)
				uncles = append(uncles, uncle.Header())
			}
		}
	}
	// Prefer to locally generated uncle
	commitUncles(w.localUncles)
	commitUncles(w.remoteUncles)

	// Fill the block with all available pending transactions.
	pending, err := w.eth.TxPool().Pending()
	if err != nil {
		log.Error("Failed to fetch pending transactions", "err", err)
		return
	}
	// Short circuit if there is no available pending transactions
	if len(pending) == 0 {
		w.updateSnapshot()
		return
	}
	// Split the pending transactions into locals and remotes
	localTxs, remoteTxs := make(map[common.Address]types.Transactions), pending
	for _, account := range w.eth.TxPool().Locals() {
		if txs := remoteTxs[account]; len(txs) > 0 {
			delete(remoteTxs, account)
			localTxs[account] = txs
		}
	}
	if len(localTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(w.current.signer, localTxs)
		if w.commitTransactions(txs, w.coinbase, interrupt) {
			return
		}
	}
	if len(remoteTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(w.current.signer, remoteTxs)
		if w.commitTransactions(txs, w.coinbase, interrupt) {
			return
		}
	}
	w.commit(uncles, w.fullTaskHook, tstart)
}

// commit runs any post-transaction state modifications, assembles the final block
// and commits new work if consensus engine is running.
func (w *worker) commit(uncles []*types.Header, interval func(), start time.Time) error {
	// Deep copy receipts here to avoid interaction between different tasks.
	receipts := make([]*types.Receipt, len(w.current.receipts))
	for i, l := range w.current.receipts {
		receipts[i] = new(types.Receipt)
		*receipts[i] = *l
	}

	// Make sure txs.L1Timestamp set to block
	pn := w.current.header.Number.Uint64() - 1
	l1BN := uint64(0)
	deSeqModel := false
	blockTime := w.current.header.Time
	log.Debug("Special info in worker: commit", "cmp to DeSeqBlock", pn+1)
	if rcfg.DeSeqBlock > 0 && pn+1 >= rcfg.DeSeqBlock {
		deSeqModel = true
	}
	if deSeqModel {
		for index, tx := range w.current.txs {
			tx.SetIndex(pn)
			tx.SetL1Timestamp(blockTime)
			if index == 0 {
				l1BN = tx.L1BlockNumber().Uint64()
			} else {
				tx.SetL1BlockNumber(l1BN)
			}
		}
	}

	s := w.current.state.Copy()
	block, err := w.engine.FinalizeAndAssemble(w.chain, w.current.header, s, w.current.txs, uncles, w.current.receipts)
	if err != nil {
		return err
	}

	// As a sanity check, ensure all new blocks have exactly one
	// transaction. This check is done here just in case any of our
	// higher-evel checks failed to catch empty blocks passed to commit.
	txs := block.Transactions()
	// New block, DeSeqBlock compare not plus 1
	log.Debug("Special info in worker: commit", "cmp to DeSeqBlock", w.current.header.Number.Uint64())
	if rcfg.UsingOVM && !(rcfg.DeSeqBlock > 0 && w.current.header.Number.Uint64() >= rcfg.DeSeqBlock) {
		if len(txs) != 1 {
			return fmt.Errorf("Block created with %d transactions rather than 1 at %d", len(txs), block.NumberU64())
		}
	} else if len(txs) == 0 {
		return fmt.Errorf("Block created with %d transactions at %d", len(txs), block.NumberU64())
	}

	if deSeqModel {
		// Update Sync Index
		w.eth.SyncService().SetLatestIndex(&pn)
		w.eth.SyncService().SetLatestIndexTime(int64(blockTime))
	}

	if w.isRunning() {
		if interval != nil {
			interval()
		}
		// Writing to the taskCh will result in the block being added to the
		// chain via the resultCh
		select {
		case w.taskCh <- &task{receipts: receipts, state: s, block: block, createdAt: time.Now()}:
			w.unconfirmed.Shift(block.NumberU64() - 1)

			feesWei := new(big.Int)
			for i, tx := range block.Transactions() {
				feesWei.Add(feesWei, new(big.Int).Mul(new(big.Int).SetUint64(receipts[i].GasUsed), tx.GasPrice()))
			}
			feesEth := new(big.Float).Quo(new(big.Float).SetInt(feesWei), new(big.Float).SetInt(big.NewInt(params.Ether)))

			tx := txs[0]
			bn := tx.L1BlockNumber()
			if bn == nil {
				bn = new(big.Int)
			}
			log.Info("New block", "index", block.Number().Uint64()-uint64(1), "l1-timestamp", tx.L1Timestamp(), "l1-blocknumber", bn.Uint64(), "tx-hash", tx.Hash().Hex(),
				"queue-orign", tx.QueueOrigin(), "gas", block.GasUsed(), "fees", feesEth, "elapsed", common.PrettyDuration(time.Since(start)))

		case <-w.exitCh:
			log.Info("Worker has exited")
		}
	}
	w.updateSnapshot()
	return nil
}

// postSideBlock fires a side chain event, only use it for testing.
func (w *worker) postSideBlock(event core.ChainSideEvent) {
	select {
	case w.chainSideCh <- event:
	case <-w.exitCh:
	}
}

func updateTransactionStateMetrics(start time.Time, state *state.StateDB) {
	accountReadTimer.Update(state.AccountReads)
	storageReadTimer.Update(state.StorageReads)
	accountUpdateTimer.Update(state.AccountUpdates)
	storageUpdateTimer.Update(state.StorageUpdates)

	triehash := state.AccountHashes + state.StorageHashes
	txExecutionTimer.Update(time.Since(start) - triehash)
}

func (w *worker) handleErrInTask(err error, headFlag bool) {
	if rcfg.UsingOVM {
		w.eth.SyncService().PushTxApplyError(err)
	}
	if headFlag {
		// w.makeEmptyChainHeadEvent()
		log.Debug("handle err in work task", "header", headFlag)
	}
}
