package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

func (a *App) masterHome(uid string) string {
	return a.master.homes.user(uid)
}

// Image servers only need identity, validity and signing state. Display names,
// biography and the user's current location are authoritative at the coordinator.
func imageAccountState(u *Account) Account {
	return Account{ID: u.ID, HomeRegion: u.HomeRegion, Secret: u.Secret, Status: u.Status, ProfileImageID: u.ProfileImageID}
}

func sameImageAccountState(a, b *Account) bool {
	if a == nil || b == nil || a.ID != b.ID || a.HomeRegion != b.HomeRegion || a.Secret != b.Secret || a.Status != b.Status {
		return false
	}
	if a.ProfileImageID == nil || b.ProfileImageID == nil {
		return a.ProfileImageID == nil && b.ProfileImageID == nil
	}
	return *a.ProfileImageID == *b.ProfileImageID
}

func (a *App) masterGroups(txn masterTransaction) map[string]masterTransaction {
	groups := map[string]masterTransaction{}
	accounts := map[string]*Account{}
	for _, c := range txn.Changes {
		if c.Kind == "account" {
			var v Account
			json.Unmarshal(c.Data, &v)
			accounts[v.ID] = &v
		}
	}
	appendChange := func(home string, c change) {
		if !validRegion(home) {
			panic("image transaction has no region")
		}
		part := groups[home]
		part.ID, part.Epoch = txn.ID, txn.Epoch
		part.Changes = append(part.Changes, c)
		groups[home] = part
	}
	sentAccounts := map[string]bool{}
	appendAccount := func(u *Account) {
		if sentAccounts[u.ID] {
			return
		}
		data, err := json.Marshal(imageAccountState(u))
		if err != nil {
			panic(err)
		}
		appendChange(u.HomeRegion, change{Kind: "account", Data: data})
		sentAccounts[u.ID] = true
	}
	for _, c := range txn.Changes {
		var home string
		switch c.Kind {
		case "account":
			var v Account
			json.Unmarshal(c.Data, &v)
			old := a.appCore.s.Accounts.Get(v.ID)
			// A new account has no images to invalidate. Its first image carries
			// the required owner record in the same regional transaction.
			if old == nil && v.Status == "active" && v.ProfileImageID == nil || sameImageAccountState(old, &v) {
				continue
			}
			appendAccount(&v)
			continue
		case "image":
			var v Image
			json.Unmarshal(c.Data, &v)
			u := accounts[v.OwnerID]
			if u == nil {
				u = a.s.Accounts.Get(v.OwnerID)
			}
			if u == nil {
				panic("image owner does not exist")
			}
			appendAccount(u)
			home = a.masterHome(v.OwnerID)
		case "post":
			var v Post
			json.Unmarshal(c.Data, &v)
			old := a.appCore.s.Posts.get(v.ID)
			if old != nil && old.AuthorID == v.AuthorID && old.ImageID == v.ImageID &&
				(old.DeletedAt == nil && v.DeletedAt == nil || old.DeletedAt != nil && v.DeletedAt != nil && old.DeletedAt.Equal(*v.DeletedAt)) {
				continue
			}
			home = a.masterHome(v.AuthorID)
		case "reaction":
			var v Reaction
			json.Unmarshal(c.Data, &v)
			home = a.masterHome(v.ReactorID)
		case "ban", "window":
			for _, region := range masterRegions {
				appendChange(region, c)
			}
			continue
		default:
			continue
		}
		appendChange(home, c)
	}
	return groups
}

func (a *App) persistMasterBatch(region string, txns []masterTransaction) {
	for {
		var err error
		if region == "asia" {
			err = a.master.commit(txns)
		} else {
			var response *http.Response
			response, err = masterRequest(context.Background(), region, "commit", txns)
			if err == nil {
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if response.StatusCode != 204 {
					err = fmt.Errorf("HTTP %d", response.StatusCode)
				}
			}
		}
		if err == nil {
			return
		}
		// The durable coordinator intent is retried after a process restart as
		// well. Never publish or acknowledge an update its master has not stored.
		log.Printf("master %s commit pending: %v", region, err)
		time.Sleep(100 * time.Millisecond)
	}
}

func (a *App) persistMasterTransaction(txn masterTransaction) {
	var group sync.WaitGroup
	for region, part := range a.masterGroups(txn) {
		group.Add(1)
		go func(region string, part masterTransaction) {
			defer group.Done()
			a.persistMasterBatch(region, []masterTransaction{part})
		}(region, part)
	}
	group.Wait()
}

func (a *App) ensureMasterEpoch(epoch string) error {
	var group sync.WaitGroup
	errors := make(chan error, len(masterRegions))
	for _, region := range masterRegions {
		group.Add(1)
		go func(region string) {
			defer group.Done()
			if region == "asia" {
				errors <- a.master.reset(epoch)
				return
			}
			resp, err := masterRequest(context.Background(), region, "reset", object{"epoch": epoch})
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 204 {
					err = fmt.Errorf("reset master %s: HTTP %d", region, resp.StatusCode)
				}
			}
			errors <- err
		}(region)
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

func coordinatorJournalPath(root, epoch string) string {
	return filepath.Join(root, "coordinator-"+epoch+".jsonl")
}

func (a *App) enableRegionalMasters(master *regionalMaster) error {
	a.master, a.mastersEnabled = master, true
	master.stats = &a.regionalImages
	epochPath := filepath.Join(a.durable, "coordinator-epoch")
	b, err := os.ReadFile(epochPath)
	if os.IsNotExist(err) {
		b = []byte(id())
		if err = writeTextAtomic(epochPath, string(b)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	a.masterEpoch = strings.TrimSpace(string(b))
	if _, ok := uuidKey(a.masterEpoch); !ok {
		return fmt.Errorf("invalid coordinator epoch")
	}
	if a.journal != nil {
		a.journal.Close()
	}
	if a.initialState == nil {
		if a.s != nil {
			a.s.closeHistory()
		}
		a.s = nil
		a.appCore.s = nil
		runtime.GC()
		if err := a.loadInitial(); err != nil {
			return err
		}
	} else {
		a.s = a.initialState.fork()
		a.appCore.s = a.s
	}
	if err := a.ensureMasterEpoch(a.masterEpoch); err != nil {
		return err
	}
	a.journal, err = os.OpenFile(coordinatorJournalPath(a.durable, a.masterEpoch), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	var entries []masterTransaction
	scan := bufio.NewScanner(a.journal)
	scan.Buffer(make([]byte, 65536), 64<<20)
	for scan.Scan() {
		var txn masterTransaction
		if err := json.Unmarshal(scan.Bytes(), &txn); err != nil {
			return err
		}
		if txn.Epoch != a.masterEpoch {
			return fmt.Errorf("coordinator journal epoch mismatch")
		}
		entries = append(entries, txn)
	}
	if err := scan.Err(); err != nil {
		return err
	}
	byRegion := map[string][]masterTransaction{}
	for _, txn := range entries {
		for region, part := range a.masterGroups(txn) {
			byRegion[region] = append(byRegion[region], part)
		}
		for _, c := range txn.Changes {
			if err := a.s.apply(c.Kind, c.Data); err != nil {
				return err
			}
		}
	}
	var replay sync.WaitGroup
	for region, txns := range byRegion {
		replay.Add(1)
		go func(region string, txns []masterTransaction) {
			defer replay.Done()
			for start := 0; start < len(txns); start += 128 {
				a.persistMasterBatch(region, txns[start:min(start+128, len(txns))])
			}
		}(region, txns)
	}
	replay.Wait()
	return nil
}

func (a *App) resetRegionalMasters() error {
	epoch := id()
	if err := writeTextAtomic(filepath.Join(a.durable, "coordinator-epoch"), epoch); err != nil {
		return err
	}
	a.masterEpoch = epoch
	a.journal.Close()
	var err error
	a.journal, err = os.OpenFile(coordinatorJournalPath(a.durable, epoch), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	if err := a.ensureMasterEpoch(epoch); err != nil {
		return err
	}
	a.s = nil
	a.appCore.s = nil
	if err := a.loadInitial(); err != nil {
		return err
	}
	a.failures = new(shardedMap[string, []time.Time])
	a.pending = nil
	a.signatureMu.Lock()
	a.signatures = map[string]string{}
	a.signatureOrder = nil
	a.signatureNext = 0
	a.signatureMu.Unlock()
	a.notifier.reset()
	old, _ := filepath.Glob(filepath.Join(a.durable, "coordinator-*.jsonl"))
	for _, path := range old {
		if path != coordinatorJournalPath(a.durable, epoch) {
			os.Remove(path)
		}
	}
	return nil
}
