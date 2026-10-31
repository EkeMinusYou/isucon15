package main

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestSnapshotBatchAdmitsWaitingReadersTogether(t *testing.T) {
	var batch snapshotBatch
	var gate sync.RWMutex
	gate.RLock()
	const count = 4
	entered := make(chan struct{}, count)
	release := make(chan struct{})
	var done sync.WaitGroup
	for range count {
		done.Add(1)
		go func() {
			defer done.Done()
			batch.lock(&gate)
			entered <- struct{}{}
			<-release
			batch.unlock(&gate)
		}()
	}
	waitQueued := func(want int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			batch.mu.Lock()
			n := batch.waiting
			batch.mu.Unlock()
			if n == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("waiting=%d, want=%d", n, want)
			}
			runtime.Gosched()
		}
	}
	waitQueued(count)
	gate.RUnlock()
	for range count {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("waiting snapshots were serialized")
		}
	}
	if gate.TryRLock() {
		gate.RUnlock()
		t.Fatal("mutation entered during a snapshot cohort")
	}
	late := make(chan struct{})
	go func() {
		batch.lock(&gate)
		close(late)
		batch.unlock(&gate)
	}()
	waitQueued(1)
	select {
	case <-late:
		t.Fatal("late reader extended the active cohort")
	default:
	}
	close(release)
	done.Wait()
	select {
	case <-late:
	case <-time.After(time.Second):
		t.Fatal("next snapshot cohort did not progress")
	}
	gate.RLock()
	gate.RUnlock()
}
