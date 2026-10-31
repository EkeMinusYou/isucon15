package main

import (
	"sync"
	"sync/atomic"
)

// A fixed cohort of collection reads shares one exclusive state-gate hold.
// This drains executing mutations without waiting for their resource queues,
// while retaining parallel snapshot capture among the admitted reads. Arrivals
// after admission wait for the next cohort so a busy read stream cannot extend
// the current hold indefinitely.
type snapshotBatch struct {
	mu         sync.Mutex
	cond       *sync.Cond
	generation uint64
	waiting    int
	active     int
	acquiring  bool
	cohorts    atomic.Uint64
	reads      atomic.Uint64
}

func (b *snapshotBatch) lock(gate *sync.RWMutex) {
	b.mu.Lock()
	if b.cond == nil {
		b.cond = sync.NewCond(&b.mu)
	}
	generation := b.generation
	b.waiting++
	for generation == b.generation {
		if b.active == 0 && !b.acquiring {
			b.acquiring = true
			b.mu.Unlock()
			gate.Lock()
			b.mu.Lock()
			b.active, b.waiting = b.waiting, 0
			b.cohorts.Add(1)
			b.reads.Add(uint64(b.active))
			b.generation++
			b.acquiring = false
			b.cond.Broadcast()
		} else {
			b.cond.Wait()
		}
	}
	b.mu.Unlock()
}

func (b *snapshotBatch) unlock(gate *sync.RWMutex) {
	b.mu.Lock()
	b.active--
	if b.active == 0 {
		// RWMutex wakes the RLock callers already queued by mutations and point
		// reads before the next cohort's Lock can complete.
		gate.Unlock()
		b.cond.Broadcast()
	}
	b.mu.Unlock()
}
