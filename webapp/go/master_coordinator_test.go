package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorPublishesOnlyAfterMasterCommit(t *testing.T) {
	oldSecret := transportSecret
	transportSecret = "master-transaction-test"
	t.Cleanup(func() { transportSecret = oldSecret })
	a := fixtureApp(t)
	local, err := newRegionalMaster(a.initial, t.TempDir(), a.initial+"/images", "asia", &regionalUploads{root: a.durable})
	if err != nil {
		t.Fatal(err)
	}
	defer local.close()
	var delayed atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	for _, region := range []string{"europe", "america"} {
		files, err := newRegionalUploads(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		master, err := newRegionalMaster(a.initial, t.TempDir(), a.initial+"/images", region, files)
		if err != nil {
			t.Fatal(err)
		}
		defer master.close()
		home := region
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if home == "europe" && r.URL.Path == masterPrefix+"commit" && delayed.Load() {
				b, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(b))
				once.Do(func() { close(entered) })
				<-release
			}
			if !master.internal(w, r) {
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		t.Setenv("REGION_"+map[string]string{"europe": "EUROPE", "america": "AMERICA"}[region]+"_URL", server.URL)
	}
	if err := a.enableRegionalMasters(local); err != nil {
		t.Fatal(err)
	}
	_, viewer := register(t, a, "master_viewer")
	registered := requireCall(t, a, "POST", "/api/auth/register", "", object{"username": "master_editor", "displayName": "before", "region": "europe"}, 201)
	user := registered["user"].(map[string]any)
	uid := user["id"].(string)
	login := requireCall(t, a, "POST", "/api/auth/login", "", object{"username": "master_editor", "secretKey": registered["secretKey"]}, 200)
	token := login["token"].(string)
	delayed.Store(true)
	finished := make(chan int, 1)
	go func() {
		b, _ := json.Marshal(object{"displayName": "after"})
		r := httptest.NewRequest("PATCH", "/api/me", bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		finished <- w.Code
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("master commit was not reached")
	}
	visible := make(chan map[string]any, 1)
	go func() { _, value, _ := call(t, a, "GET", "/api/users/"+uid, viewer, nil); visible <- value }()
	select {
	case value := <-visible:
		if value["displayName"] != "before" {
			t.Fatal("uncommitted master update leaked")
		}
	case <-time.After(time.Second):
		t.Fatal("master RPC blocked unrelated committed reads")
	}
	delayed.Store(false)
	close(release)
	if status := <-finished; status != 200 {
		t.Fatalf("update status %d", status)
	}
	updated := requireCall(t, a, "GET", "/api/users/"+uid, viewer, nil, 200)
	if updated["displayName"] != "after" {
		t.Fatal("committed master update was not published")
	}
	restored, err := newApp(a.initial, a.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.s.closeHistory()
	if err := restored.enableRegionalMasters(local); err != nil {
		t.Fatal(err)
	}
	if restored.s.Accounts.Get(uid).DisplayName != "after" || restored.s.Sessions.Get(token) != uid {
		t.Fatal("coordinator intent replay lost master state")
	}
}
