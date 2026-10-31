package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestPreparedChangesPublishAfterPersistence(t *testing.T) {
	a := fixtureApp(t)
	alice, token := register(t, a, "published_alice")
	bob, _ := register(t, a, "published_bob")
	subscription := a.notifier.subscribe(alice, "asia")
	defer subscription.close()
	userSignal, _ := subscription.channels()
	txn := &App{appCore: a.appCore}
	unlock := txn.lockRequest(httptest.NewRequest("POST", "/api/admin/posting-window", nil))
	journalHeld := false
	var committed chan struct{}
	defer func() {
		txn.endPreparation()
		if journalHeld {
			a.journalMu.Unlock()
		}
		if committed != nil {
			<-committed
		}
		unlock()
	}()
	txn.beginPreparation()
	txn.put("friend", Relation{ID: id(), User1ID: alice, User2ID: bob})
	profile := *txn.s.Accounts.Get(alice)
	profile.DisplayName = "published together"
	txn.put("account", profile)
	txn.signalUser(alice)
	read := func(app *App, path string) map[string]any {
		t.Helper()
		done := make(chan map[string]any, 1)
		go func() { done <- requireCall(t, app, "GET", path, token, nil, 200) }()
		select {
		case value := <-done:
			return value
		case <-time.After(time.Second):
			t.Fatal("read waited for an unpublished transaction")
			return nil
		}
	}
	if got := read(a, "/api/relationships/friends"); len(got["items"].([]any)) != 0 {
		t.Fatal("prepared relationship leaked")
	}
	if got := read(a, "/api/me"); got["displayName"] != "published_alice" {
		t.Fatal("prepared account leaked")
	}
	select {
	case <-userSignal:
		t.Fatal("notification preceded publication")
	default:
	}
	txn.endPreparation()
	a.journalMu.Lock()
	journalHeld = true
	committed = make(chan struct{})
	go func() { defer close(committed); txn.commit() }()
	if got := read(a, "/api/me"); got["displayName"] != "published_alice" {
		t.Fatal("unpersisted account leaked")
	}
	a.journalMu.Unlock()
	journalHeld = false
	<-committed
	committed = nil
	if got := read(a, "/api/me"); got["displayName"] != profile.DisplayName {
		t.Fatal("account not published")
	}
	if got := read(a, "/api/relationships/friends"); len(got["items"].([]any)) != 1 {
		t.Fatal("relationship not published")
	}
	select {
	case <-userSignal:
	default:
		t.Fatal("publication did not signal its subscriber")
	}
	restored, err := newApp(a.initial, a.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.s.closeHistory()
	if got := read(restored, "/api/me"); got["displayName"] != profile.DisplayName {
		t.Fatal("published account was not recovered")
	}
	if got := read(restored, "/api/relationships/friends"); len(got["items"].([]any)) != 1 {
		t.Fatal("published relationship was not recovered")
	}
}
