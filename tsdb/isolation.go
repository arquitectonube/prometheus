// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tsdb

import (
	"math"
	"sync"
)

// isolationState holds the isolation information.
type isolationState struct {
	// We will ignore all appends above the max, or that are incomplete.
	maxAppendID       uint64
	incompleteAppends map[uint64]struct{}
	lowWatermark      uint64 // Lowest of incompleteAppends/maxAppendID.
	isolation         *isolation
	mint, maxt        int64 // Time ranges of the read.

	// Doubly linked list of active reads.
	next *isolationState
	prev *isolationState
}

// Close closes the state.
func (i *isolationState) Close() {
	i.isolation.readMtx.Lock()
	defer i.isolation.readMtx.Unlock()
	i.next.prev = i.prev
	i.prev.next = i.next
}

func (i *isolationState) IsolationDisabled() bool {
	return i.isolation.disabled
}

type isolationAppender struct {
	appendID uint64
	minTime  int64
	prev     *isolationAppender
	next     *isolationAppender
}

// isolation is the global isolation state.
type isolation struct {
	// Mutex for accessing lastAppendID and appendsOpen.
	appendMtx sync.RWMutex
	// Which appends are currently in progress.
	appendsOpen map[uint64]*isolationAppender
	// New appenders with higher appendID are added to the end. First element keeps lastAppendId.
	// appendsOpenList.next points to the first element and appendsOpenList.prev points to the last element.
	// If there are no appenders, both point back to appendsOpenList.
	appendsOpenList *isolationAppender
	// Pool of reusable *isolationAppender to save on allocations.
	appendersPool sync.Pool

	// Mutex for accessing readsOpen.
	// If taking both appendMtx and readMtx, take appendMtx first.
	readMtx sync.RWMutex
	// All current in use isolationStates. This is a doubly-linked list.
	readsOpen *isolationState
	// If true, writes are not tracked while reads are still tracked.
	disabled bool
}

func newIsolation(disabled bool) *isolation {
	isoState := &isolationState{}
	isoState.next = isoState
	isoState.prev = isoState

	appender := &isolationAppender{}
	appender.next = appender
	appender.prev = appender

	return &isolation{
		appendsOpen:     map[uint64]*isolationAppender{},
		appendsOpenList: appender,
		readsOpen:       isoState,
		disabled:        disabled,
		appendersPool:   sync.Pool{New: func() interface{} { return &isolationAppender{} }},
	}
}

// lowWatermark returns the appendID below which we no longer need to track
// which appends were from which appendID.
func (i *isolation) lowWatermark() uint64 {
	if i.disabled {
		return 0
	}

	i.appendMtx.RLock() // Take appendMtx first.
	defer i.appendMtx.RUnlock()
	return i.lowWatermarkLocked()
}

func (i *isolation) lowWatermarkLocked() uint64 {
	if i.disabled {
		return 0
	}

	i.readMtx.RLock()
	defer i.readMtx.RUnlock()
	if i.readsOpen.prev != i.readsOpen {
		return i.readsOpen.prev.lowWatermark
	}

	// Lowest appendID from appenders, or lastAppendId.
	return i.appendsOpenList.next.appendID
}

// lowestAppendTime returns the lowest minTime for any open appender,
// or math.MaxInt64 if no open appenders.
func (i *isolation) lowestAppendTime() int64 {
	var lowest int64 = math.MaxInt64
	i.appendMtx.RLock()
	defer i.appendMtx.RUnlock()

	for a := i.appendsOpenList.next; a != i.appendsOpenList; a = a.next {
		if lowest > a.minTime {
			lowest = a.minTime
		}
	}
	return lowest
}

// State returns an object used to control isolation
// between a query and appends. Must be closed when complete.
func (i *isolation) State(mint, maxt int64) *isolationState {
	i.appendMtx.RLock() // Take append mutex before read mutex.
	defer i.appendMtx.RUnlock()

	// We need to track reads even when isolation is disabled, so that head
	// truncation can wait till reads overlapping that range have finished.
	isoState := &isolationState{
		maxAppendID:       i.appendsOpenList.appendID,
		lowWatermark:      i.appendsOpenList.next.appendID, // Lowest appendID from appenders, or lastAppendId.
		incompleteAppends: make(map[uint64]struct{}, len(i.appendsOpen)),
		isolation:         i,
		mint:              mint,
		maxt:              maxt,
	}
	for k := range i.appendsOpen {
		isoState.incompleteAppends[k] = struct{}{}
	}

	i.readMtx.Lock()
	defer i.readMtx.Unlock()
	isoState.prev = i.readsOpen
	isoState.next = i.readsOpen.next
	i.readsOpen.next.prev = isoState
	i.readsOpen.next = isoState

	return isoState
}

// TraverseOpenReads iterates through the open reads and runs the given
// function on those states. The given function MUST NOT mutate the isolationState.
// The iteration is stopped when the function returns false or once all reads have been iterated.
func (i *isolation) TraverseOpenReads(f func(s *isolationState) bool) {
	i.readMtx.RLock()
	defer i.readMtx.RUnlock()
	s := i.readsOpen.next
	for s != i.readsOpen {
		if !f(s) {
			return
		}
		s = s.next
	}
}

// newAppendID increments the transaction counter and returns a new transaction
// ID. The first ID returned is 1.
// Also returns the low watermark, to keep lock/unlock operations down.
func (i *isolation) newAppendID(minTime int64) (uint64, uint64) {
	if i.disabled {
		return 0, 0
	}

	i.appendMtx.Lock()
	defer i.appendMtx.Unlock()

	// Last used appendID is stored in head element.
	i.appendsOpenList.appendID++

	app := i.appendersPool.Get().(*isolationAppender)
	app.appendID = i.appendsOpenList.appendID
	app.minTime = minTime
	app.prev = i.appendsOpenList.prev
	app.next = i.appendsOpenList

	i.appendsOpenList.prev.next = app
	i.appendsOpenList.prev = app

	i.appendsOpen[app.appendID] = app
	return app.appendID, i.lowWatermarkLocked()
}

func (i *isolation) lastAppendID() uint64 {
	if i.disabled {
		return 0
	}

	i.appendMtx.RLock()
	defer i.appendMtx.RUnlock()

	return i.appendsOpenList.appendID
}

func (i *isolation) closeAppend(appendID uint64) {
	if i.disabled {
		return
	}

	i.appendMtx.Lock()
	defer i.appendMtx.Unlock()

	app := i.appendsOpen[appendID]
	if app != nil {
		app.prev.next = app.next
		app.next.prev = app.prev

		delete(i.appendsOpen, appendID)

		// Clear all fields, and return to the pool.
		*app = isolationAppender{}
		i.appendersPool.Put(app)
	}
}

// The transactionID ring buffer.
type txRing struct {
	txIDs     []uint64
	txIDFirst uint32 // Position of the first id in the ring.
	txIDCount uint32 // How many ids in the ring.
}

func newTxRing(capacity int) *txRing {
	return &txRing{
		txIDs: make([]uint64, capacity),
	}
}

func (txr *txRing) add(appendID uint64) {
	if int(txr.txIDCount) == len(txr.txIDs) {
		// Ring buffer is full, expand by doubling.
		newLen := txr.txIDCount * 2
		if newLen == 0 {
			newLen = 4
		}
		newRing := make([]uint64, newLen)
		idx := copy(newRing, txr.txIDs[txr.txIDFirst:])
		copy(newRing[idx:], txr.txIDs[:txr.txIDFirst])
		txr.txIDs = newRing
		txr.txIDFirst = 0
	}

	txr.txIDs[int(txr.txIDFirst+txr.txIDCount)%len(txr.txIDs)] = appendID
	txr.txIDCount++
}

func (txr *txRing) cleanupAppendIDsBelow(bound uint64) {
	if len(txr.txIDs) == 0 {
		return
	}
	pos := int(txr.txIDFirst)

	for txr.txIDCount > 0 {
		if txr.txIDs[pos] >= bound {
			break
		}
		txr.txIDFirst++
		txr.txIDCount--

		pos++
		if pos == len(txr.txIDs) {
			pos = 0
		}
	}

	txr.txIDFirst %= uint32(len(txr.txIDs))
}

func (txr *txRing) iterator() *txRingIterator {
	return &txRingIterator{
		pos: txr.txIDFirst,
		ids: txr.txIDs,
	}
}

// txRingIterator lets you iterate over the ring. It doesn't terminate,
// it DOESN'T terminate.
type txRingIterator struct {
	ids []uint64

	pos uint32
}

func (it *txRingIterator) At() uint64 {
	return it.ids[it.pos]
}

func (it *txRingIterator) Next() {
	it.pos++
	if int(it.pos) == len(it.ids) {
		it.pos = 0
	}
}
