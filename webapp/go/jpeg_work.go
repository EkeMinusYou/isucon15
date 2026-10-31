package main

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"isuconapp/internal/jpegvalidate"
)

type jpegJob struct {
	ctx    context.Context
	data   []byte
	queued time.Time
	result chan bool
}

var jpegOnce sync.Once
var jpegQueue chan jpegJob
var jpegQueued atomic.Int64
var jpegActive atomic.Int64
var jpegRequests atomic.Uint64
var jpegCancelled atomic.Uint64
var jpegWaitNS atomic.Uint64
var jpegWorkNS atomic.Uint64

// One FIFO validation worker leaves the other CPU available for state/API
// progress during a simultaneous burst of large uploads.
func validateLargeJPEG(ctx context.Context, data []byte) bool {
	jpegOnce.Do(func() {
		jpegQueue = make(chan jpegJob, 256)
		go func() {
			for job := range jpegQueue {
				jpegQueued.Add(-1)
				jpegWaitNS.Add(uint64(time.Since(job.queued)))
				if job.ctx.Err() != nil {
					job.result <- false
					continue
				}
				jpegActive.Store(1)
				started := time.Now()
				valid := jpegvalidate.Validate(bytes.NewReader(job.data)) == nil
				jpegWorkNS.Add(uint64(time.Since(started)))
				jpegActive.Store(0)
				job.result <- valid
			}
		}()
	})
	jpegRequests.Add(1)
	job := jpegJob{ctx, data, time.Now(), make(chan bool, 1)}
	jpegQueued.Add(1)
	select {
	case jpegQueue <- job:
	case <-ctx.Done():
		jpegQueued.Add(-1)
		jpegCancelled.Add(1)
		return false
	}
	select {
	case valid := <-job.result:
		return valid
	case <-ctx.Done():
		jpegCancelled.Add(1)
		return false
	}
}
