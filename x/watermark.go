/*
 * Copyright (C) 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package x

import (
	"container/heap"
	"sync/atomic"

	"golang.org/x/net/trace"
)

type uint64Heap []uint64

func (u uint64Heap) Len() int               { return len(u) }
func (u uint64Heap) Less(i int, j int) bool { return u[i] < u[j] }
func (u uint64Heap) Swap(i int, j int)      { u[i], u[j] = u[j], u[i] }
func (u *uint64Heap) Push(x interface{})    { *u = append(*u, x.(uint64)) }
func (u *uint64Heap) Pop() interface{} {
	old := *u
	n := len(old)
	x := old[n-1]
	*u = old[0 : n-1]
	return x
}

// RaftValue contains the raft group and the raft proposal id.
// This is attached to the context, so the information could be passed
// down to the many posting lists, involved in mutations.
type RaftValue struct {
	Group uint32
	Index uint64
}

// TODO: make private
// Mark contains raft proposal id and a done boolean. It is used to
// update the WaterMark struct about the status of a proposal.
type Mark struct {
	Index uint64
	// Either this is an (Index, Wait) pair or (Index, Done) or (Indices, Done). Wait has to be
	// passed in the same channel because of strange logic where we notify waiters if WaitingFor()
	// becomes false

	// TODO: I think we have a bug where if we wait for syncs, but if the watermark was slow
	// getting sent over the markCh, we'll successfully return that a sync happened right away.
	Wait    chan struct{}
	Indices []uint64
	Done    bool // Set to true if the pending mutation is done.
}

// TODO: Adjust this comment.
// WaterMark is used to keep track of the maximum done index. The right way to use
// this is to send a Mark with Done set to false, as soon as an index is known.
// WaterMark will store the index in a min-heap. It would only advance, if the minimum
// entry in the heap has been successfully done.
//
// Some time later, when this index task is completed, send another Mark, this time
// with Done set to true. It would mark the index as done, and so the min-heap can
// now advance and update the maximum done water mark.
type WaterMark struct {
	Name       string
	markCh     chan Mark
	doneUntil  uint64
	elog       trace.EventLog
	waitingFor uint32 // Are we waiting for some index?
}

// Init initializes a WaterMark struct. MUST be called before using it.
func (w *WaterMark) Init() {
	w.markCh = make(chan Mark, 10000)
	w.elog = trace.NewEventLog("Watermark", w.Name)
	go w.process()
}

func (w *WaterMark) Begin(index uint64) {
	w.markCh <- Mark{Index: index, Done: false}
}
func (w *WaterMark) BeginMany(indices []uint64) {
	w.markCh <- Mark{Index: 0, Indices: indices, Done: false}
}

func (w *WaterMark) Done(index uint64) {
	w.markCh <- Mark{Index: index, Done: true}
}
func (w *WaterMark) DoneMany(indices []uint64) {
	w.markCh <- Mark{Index: 0, Indices: indices, Done: true}
}

// DoneUntil returns the maximum index until which all tasks are done.
func (w *WaterMark) DoneUntil() uint64 {
	return atomic.LoadUint64(&w.doneUntil)
}

func (w *WaterMark) SetDoneUntil(val uint64) {
	atomic.StoreUint64(&w.doneUntil, val)
}

// WaitingFor returns whether we are waiting for a task to be done.
func (w *WaterMark) WaitingFor() bool {
	return atomic.LoadUint32(&w.waitingFor) != 0
}

func (w *WaterMark) WaitForMark(index uint64) {
	// TODO: Why would we use w.WaitingFor at all?  Very questionable.
	if !w.WaitingFor() {
		return
	}

	doneUntil := w.DoneUntil()
	if doneUntil >= index {
		return
	}
	waitCh := make(chan struct{})
	w.markCh <- Mark{Index: index, Wait: waitCh}
	<-waitCh
	return
}

// process is used to process the Mark channel. This is not thread-safe,
// so only run one goroutine for process. One is sufficient, because
// all goroutine ops use purely memory and cpu.
func (w *WaterMark) process() {
	var indices, waiterIndices uint64Heap
	// pending maps raft proposal index to the number of pending mutations for this proposal.
	pending := make(map[uint64]int)
	waiters := make(map[uint64][]chan struct{})

	heap.Init(&indices)
	heap.Init(&waiterIndices)
	var loop uint64

	processOne := func(index uint64, done bool) {
		// If not already done, then set. Otherwise, don't undo a done entry.
		prev, present := pending[index]
		if !present {
			heap.Push(&indices, index)
			// indices now nonempty, update waitingFor.
			atomic.StoreUint32(&w.waitingFor, 1)
		}

		delta := 1
		if done {
			delta = -1
		}
		pending[index] = prev + delta

		loop++
		if len(indices) > 0 && loop%10000 == 0 {
			min := indices[0]
			w.elog.Printf("WaterMark %s: Done entry %4d. Size: %4d Watermark: %-4d Looking for: %-4d. Value: %d\n",
				w.Name, index, len(indices), w.DoneUntil(), min, pending[min])
		}

		// Update mark by going through all indices in order; and checking if they have
		// been done. Stop at the first index, which isn't done.
		doneUntil := w.DoneUntil()
		AssertTrue(doneUntil < index)

		until := doneUntil
		loops := 0
		var doWait bool

		for len(indices) > 0 {
			min := indices[0]
			if done := pending[min]; done != 0 {
				doWait = true
				break // len(indices) will be > 0.
			}
			heap.Pop(&indices)
			delete(pending, min)
			until = min
			loops++
		}
		if !doWait {
			atomic.StoreUint32(&w.waitingFor, 0)
			// TODO: Some super-gross duplicated code, yes.
			for len(waiterIndices) > 0 {
				min := waiterIndices[0]
				heap.Pop(&waiterIndices)
				toNotify := waiters[min]
				for _, ch := range toNotify {
					close(ch)
				}
				delete(waiters, min)
			}
		}

		if until != doneUntil {
			for len(waiterIndices) > 0 {
				min := waiterIndices[0]
				if min > until {
					break
				}
				heap.Pop(&waiterIndices)
				toNotify := waiters[min]
				for _, ch := range toNotify {
					close(ch)
				}
				delete(waiters, min)
			}
			// TODO: To be consistent with what we had before, we should also notify ALL waiters if !doWait.

			AssertTrue(atomic.CompareAndSwapUint64(&w.doneUntil, doneUntil, until))
			w.elog.Printf("%s: Done until %d. Loops: %d\n", w.Name, until, loops)
		}
	}

	for mark := range w.markCh {
		if mark.Wait != nil {
			doneUntil := atomic.LoadUint64(&w.doneUntil)
			if doneUntil >= mark.Index {
				close(mark.Wait)
			} else {
				ws, ok := waiters[mark.Index]
				if !ok {
					heap.Push(&waiterIndices, mark.Index)
					waiters[mark.Index] = []chan struct{}{mark.Wait}
				} else {
					waiters[mark.Index] = append(ws, mark.Wait)
				}
			}
		} else {
			if mark.Index > 0 {
				processOne(mark.Index, mark.Done)
			}
			for _, index := range mark.Indices {
				processOne(index, mark.Done)
			}
		}
	}
}
