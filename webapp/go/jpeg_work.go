package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
var jpegRemoteRequests atomic.Uint64
var jpegRemoteFailures atomic.Uint64

// Keep one local CPU for validation and use independent workers on the nearby
// compute host. All workers consume one FIFO queue and validate the same bytes.
func validateLargeJPEG(ctx context.Context, data []byte) bool {
	jpegOnce.Do(func() {
		jpegQueue = make(chan jpegJob, 256)
		worker := func(remote bool) {
			for job := range jpegQueue {
				jpegQueued.Add(-1)
				jpegWaitNS.Add(uint64(time.Since(job.queued)))
				if job.ctx.Err() != nil {
					job.result <- false
					continue
				}
				jpegActive.Add(1)
				started := time.Now()
				valid := false
				var err error
				if remote {
					jpegRemoteRequests.Add(1)
					valid, err = remoteJPEG(job.ctx, job.data)
					if err != nil {
						jpegRemoteFailures.Add(1)
					}
				}
				if !remote || err != nil {
					valid = jpegvalidate.Validate(bytes.NewReader(job.data)) == nil
				}
				jpegWorkNS.Add(uint64(time.Since(started)))
				jpegActive.Add(-1)
				job.result <- valid
			}
		}
		go worker(false)
		if os.Getenv("COMPUTE_URL") != "" {
			go worker(true)
			go worker(true)
		}
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

func remoteJPEG(ctx context.Context, data []byte) (bool, error) {
	resp, err := computeRequest(ctx, "jpeg", data)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("JPEG worker status %d", resp.StatusCode)
	}
	var valid bool
	err = json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&valid)
	return valid, err
}
