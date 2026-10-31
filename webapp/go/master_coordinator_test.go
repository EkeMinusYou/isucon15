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

func TestCentralReadsDoNotQueryRegionalState(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "central_reader")
	local, err := newRegionalMaster(a.initial, t.TempDir(), a.initial+"/images", "asia", &regionalUploads{root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer local.close()
	a.master, a.mastersEnabled = local, true
	var calls atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "regional state unavailable", 503)
	}))
	defer remote.Close()
	t.Setenv("REGION_EUROPE_URL", remote.URL)
	t.Setenv("REGION_AMERICA_URL", remote.URL)
	for _, path := range []string{"/api/users/" + uid, "/api/feeds/friends"} {
		requireCall(t, a, "GET", path, token, nil, 200)
	}
	if calls.Load() != 0 {
		t.Fatal("central read queried regional state")
	}
}

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
	var regionalCommits atomic.Int64
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var europe *regionalMaster
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
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
		if region == "europe" {
			europe = master
		}
		home := region
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == masterPrefix+"commit" {
				regionalCommits.Add(1)
			}
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
		defer func() { releaseOnce.Do(func() { close(release) }); server.Close() }()
		t.Setenv("REGION_"+map[string]string{"europe": "EUROPE", "america": "AMERICA"}[region]+"_URL", server.URL)
	}
	if err := a.enableRegionalMasters(local); err != nil {
		t.Fatal(err)
	}
	beforeCommits := regionalCommits.Load()
	_, viewer := register(t, a, "master_viewer")
	registered := requireCall(t, a, "POST", "/api/auth/register", "", object{"username": "master_editor", "displayName": "before", "region": "europe"}, 201)
	user := registered["user"].(map[string]any)
	uid := user["id"].(string)
	ip := "192.0.2.1"
	ipLock := testResource(t, a, "ip:"+ip)
	ipLock.Lock()
	ipHeld := true
	defer func() {
		if ipHeld {
			ipLock.Unlock()
		}
	}()
	loggedIn := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		b, _ := json.Marshal(object{"username": "master_editor", "secretKey": registered["secretKey"]})
		r := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(b))
		r.Header.Set("X-Client-IP", ip)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		loggedIn <- w
	}()
	var response *httptest.ResponseRecorder
	select {
	case response = <-loggedIn:
	case <-time.After(time.Second):
		ipLock.Unlock()
		ipHeld = false
		<-loggedIn
		t.Fatal("validated login waited for an IP failure counter")
	}
	ipLock.Unlock()
	ipHeld = false
	if response.Code != 200 {
		t.Fatalf("login returned %d: %s", response.Code, response.Body.String())
	}
	var login object
	if err := json.Unmarshal(response.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	token := login["token"].(string)
	if europe.s.Sessions.Get(token) != "" {
		t.Fatal("central session was replicated to an image master")
	}
	requireCall(t, a, "PATCH", "/api/me", token, object{"displayName": "after"}, 200)
	if regionalCommits.Load() != beforeCommits {
		t.Fatal("registration or text-only profile update reached image replicas")
	}
	delayed.Store(true)
	finished := make(chan int, 1)
	go func() {
		r := httptest.NewRequest("POST", "/api/me/account-deletion", nil)
		r.Header.Set("X-ISU-Region", "europe")
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
	go func() {
		_, value, _ := call(t, a, "GET", "/api/users/"+uid, viewer, nil)
		visible <- value
	}()
	select {
	case value := <-visible:
		if value["displayName"] != "after" {
			t.Fatal("uncommitted master update leaked")
		}
	case <-time.After(time.Second):
		t.Fatal("master RPC blocked unrelated committed reads")
	}
	delayed.Store(false)
	releaseOnce.Do(func() { close(release) })
	if status := <-finished; status != 204 {
		t.Fatalf("update status %d", status)
	}
	requireCall(t, a, "GET", "/api/users/"+uid, viewer, nil, 404)
	restored, err := newApp(a.initial, a.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.closeHistory()
	if err := restored.enableRegionalMasters(local); err != nil {
		t.Fatal(err)
	}
	if restored.s.Accounts.Get(uid).Status != "deleted" || restored.s.Sessions.Get(token) != "" {
		t.Fatal("coordinator intent replay lost master state")
	}
	initialView, initialMaster := restored.initialState, local.initialState
	if initialView.Accounts.Get(uid) != nil || initialMaster.Accounts.Get(uid) != nil {
		t.Fatal("an update mutated immutable initialization state")
	}
	requireCall(t, restored, "POST", "/api/initialize", "", nil, 200)
	if restored.initialState != initialView || local.initialState != initialMaster {
		t.Fatal("initialize reloaded immutable history")
	}
	if restored.appCore.s.Accounts.Get(uid) != nil || local.s.Accounts.Get(uid) != nil {
		t.Fatal("initialize retained an update")
	}
}
