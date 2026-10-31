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

// Each batch starts its master reads after every waiter in that batch arrived.
// A later read joins the next batch, so a completed write cannot be missed.
func (a *App) ensureFresh(ctx context.Context) error {
	start := time.Now()
	defer func() { a.mirrorWaitNS.Add(uint64(time.Since(start))) }()
	done := make(chan error, 1)
	a.mirror.mu.Lock()
	a.mirror.waiting = append(a.mirror.waiting, done)
	if !a.mirror.running {
		a.mirror.running = true
		go a.refreshLoop()
	}
	a.mirror.mu.Unlock()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) refreshLoop() {
	for {
		a.mirror.mu.Lock()
		batch := a.mirror.waiting
		a.mirror.waiting = nil
		if len(batch) == 0 {
			a.mirror.running = false
			a.mirror.mu.Unlock()
			return
		}
		a.mirror.mu.Unlock()
		err := a.pullFresh()
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

func (a *App) pullFresh() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	epoch := a.masterEpoch
	var records [3][]masterTransaction
	var errs [3]error
	var group sync.WaitGroup
	for index, region := range masterRegions {
		group.Add(1)
		go func(index int, region string) {
			defer group.Done()
			cursor := a.mirrored[index]
			var target uint64
			first := true
			for {
				result, err := a.fetchMaster(epoch, region, cursor)
				if err != nil {
					errs[index] = err
					return
				}
				if result.Epoch != epoch || result.Through < cursor {
					errs[index] = fmt.Errorf("invalid master cursor")
					return
				}
				if first {
					target = result.Head
					first = false
				}
				for _, txn := range result.Transactions {
					if txn.Source != region || txn.Epoch != epoch || txn.Sequence != cursor+1 {
						errs[index] = fmt.Errorf("invalid master sequence")
						return
					}
					records[index] = append(records[index], txn)
					cursor = txn.Sequence
				}
				if cursor != result.Through {
					errs[index] = fmt.Errorf("incomplete master batch")
					return
				}
				if cursor >= target {
					return
				}
				if len(result.Transactions) == 0 {
					errs[index] = fmt.Errorf("master made no progress")
					return
				}
			}
		}(index, region)
	}
	group.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	var buffer bytes.Buffer
	count := 0
	for _, batch := range records {
		for _, txn := range batch {
			if err := json.NewEncoder(&buffer).Encode(txn); err != nil {
				return err
			}
			count++
		}
	}
	if count == 0 {
		return nil
	}
	a.globalGate.Lock()
	defer a.globalGate.Unlock()
	a.journalMu.Lock()
	_, err := a.journal.Write(buffer.Bytes())
	a.journalMu.Unlock()
	if err != nil {
		return err
	}
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	for index, batch := range records {
		for _, txn := range batch {
			for _, change := range txn.Changes {
				if err := a.appCore.s.apply(change.Kind, change.Data); err != nil {
					return err
				}
			}
			a.mirrored[index] = txn.Sequence
		}
	}
	a.mirrorRecords.Add(uint64(count))
	return nil
}
