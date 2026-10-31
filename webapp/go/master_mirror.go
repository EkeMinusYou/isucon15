package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type mirrorQueue struct {
	mu      sync.Mutex
	running bool
	waiting []chan error
}

func needsMirror(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/api/internal/") && !strings.HasPrefix(r.URL.Path, "/api/admin/") && r.URL.Path != "/api/initialize" && r.URL.Path != "/api/auth/register"
}

// Only sessions, posts and post images can be changed without the coordinator.
// All other records are synchronously published by their coordinator update.
func (a *App) mirrorRegions(r *http.Request) uint8 {
	mask := uint8(0)
	add := func(region string) {
		if code := masterRegionCode(region); code != 0 {
			mask |= 1 << (code - 1)
		}
	}
	add(homeOfNewID(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/api/feeds/"), strings.HasPrefix(path, "/api/comments/"), strings.HasPrefix(path, "/api/realmojis/"):
		return 7
	case strings.HasPrefix(path, "/api/posts/") && path != "/api/posts/me/current":
		pid := strings.Split(strings.TrimPrefix(path, "/api/posts/"), "/")[0]
		add(indexedHome(a.master.homes.posts, pid))
	}
	return mask
}

// Each region starts its read after every waiter in that batch arrived.
// Independent queues keep unrelated regions out of the read's critical path.
func (a *App) ensureFresh(ctx context.Context, mask uint8) error {
	start := time.Now()
	defer func() { a.mirrorWaitNS.Add(uint64(time.Since(start))) }()
	done := make(chan error, len(masterRegions))
	count := 0
	for index := range masterRegions {
		if mask&(1<<index) == 0 {
			continue
		}
		count++
		queue := &a.mirror[index]
		queue.mu.Lock()
		queue.waiting = append(queue.waiting, done)
		if !queue.running {
			queue.running = true
			go a.refreshLoop(index)
		}
		queue.mu.Unlock()
	}
	for ; count > 0; count-- {
		select {
		case err := <-done:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (a *App) refreshLoop(index int) {
	queue := &a.mirror[index]
	for {
		queue.mu.Lock()
		batch := queue.waiting
		queue.waiting = nil
		if len(batch) == 0 {
			queue.running = false
			queue.mu.Unlock()
			return
		}
		queue.mu.Unlock()
		err := a.pullFresh(index)
		for _, done := range batch {
			done <- err
		}
	}
}

func (a *App) fetchMaster(epoch, region string, after uint64) (masterPull, error) {
	a.mirrorPulls.Add(1)
	if region == "asia" {
		return a.master.pull(epoch, after)
	}
	response, err := masterRequest(context.Background(), region, "pull?epoch="+url.QueryEscape(epoch)+"&after="+strconv.FormatUint(after, 10), nil)
	if err != nil {
		return masterPull{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		io.Copy(io.Discard, response.Body)
		return masterPull{}, fmt.Errorf("master pull %s: HTTP %d", region, response.StatusCode)
	}
	var result masterPull
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return masterPull{}, err
	}
	return result, nil
}

func (a *App) pullFresh(index int) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	epoch := a.masterEpoch
	region := masterRegions[index]
	var records []masterTransaction
	cursor := a.mirrored[index]
	var target uint64
	first := true
	for {
		result, err := a.fetchMaster(epoch, region, cursor)
		if err != nil {
			return err
		}
		if result.Epoch != epoch || result.Through < cursor {
			return fmt.Errorf("invalid master cursor")
		}
		if first {
			target = result.Head
			first = false
		}
		for _, txn := range result.Transactions {
			if txn.Source != region || txn.Epoch != epoch || txn.Sequence != cursor+1 {
				return fmt.Errorf("invalid master sequence")
			}
			records = append(records, txn)
			cursor = txn.Sequence
		}
		if cursor != result.Through {
			return fmt.Errorf("incomplete master batch")
		}
		if cursor >= target {
			break
		}
		if len(result.Transactions) == 0 {
			return fmt.Errorf("master made no progress")
		}
	}
	var buffer bytes.Buffer
	for _, txn := range records {
		if err := json.NewEncoder(&buffer).Encode(txn); err != nil {
			return err
		}
	}
	if len(records) == 0 {
		return nil
	}
	// Regional events only change owned sessions, posts and their new images.
	// Exclude window switches, but do not drain unrelated resource updates.
	// The committed-view lock makes the publication itself atomic.
	a.globalGate.RLock()
	defer a.globalGate.RUnlock()
	a.journalMu.Lock()
	_, err := a.journal.Write(buffer.Bytes())
	a.journalMu.Unlock()
	if err != nil {
		return err
	}
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	for _, txn := range records {
		for _, change := range txn.Changes {
			if err := a.appCore.s.apply(change.Kind, change.Data); err != nil {
				return err
			}
		}
		a.mirrored[index] = txn.Sequence
	}
	a.mirrorRecords.Add(uint64(len(records)))
	return nil
}
