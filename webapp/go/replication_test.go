package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type replicationFixture struct {
	primary               *App
	server                *httptest.Server
	apps                  map[string]*App
	handlers              map[string]http.Handler
	servers               map[string]*httptest.Server
	stateDirs, uploadDirs map[string]string
	blockAck              atomic.Bool
	releaseAck            chan struct{}
	releaseOnce           sync.Once
	business              atomic.Uint64
	keyFile               string
}

func awaitReplica(t *testing.T, app *App) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !app.replica.ready.Load() {
		if time.Now().After(deadline) {
			t.Fatal("replica did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
}
func newReplicationFixture(t *testing.T) *replicationFixture {
	t.Helper()
	oldSecret, oldRegion := transportSecret, fixedRegion
	transportSecret, fixedRegion = "replication-test", "asia"
	t.Cleanup(func() { transportSecret, fixedRegion = oldSecret, oldRegion })
	f := &replicationFixture{primary: fixtureApp(t), apps: map[string]*App{}, handlers: map[string]http.Handler{}, servers: map[string]*httptest.Server{}, stateDirs: map[string]string{}, uploadDirs: map[string]string{}, releaseAck: make(chan struct{})}
	var err error
	f.primary.replication, err = newReplicationHub(f.primary, []string{"europe", "america"})
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == replicationAckPath && f.blockAck.Load() {
			select {
			case <-f.releaseAck:
			case <-r.Context().Done():
				return
			}
		}
		if !strings.HasPrefix(r.URL.Path, "/api/internal/replication/") {
			f.business.Add(1)
		}
		if f.primary.replicationHTTP(w, r) || f.primary.ownedOriginalHTTP(w, r) {
			return
		}
		if !isInternal(r) {
			r.Header.Set("X-ISU-Region", "asia")
		}
		f.primary.ServeHTTP(w, r)
	}))
	f.handlers["asia"] = f.server.Config.Handler
	f.keyFile = filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(f.keyFile, []byte(transportSecret), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REGIONAL_READS", "1")
	t.Setenv("OWNER_URL", f.server.URL)
	t.Setenv("COMPUTE_URL", "")
	t.Setenv("INITIAL_DIR", f.primary.initial)
	t.Setenv("TRANSPORT_KEY_FILE", f.keyFile)
	t.Setenv("REGION_ASIA_URL", f.server.URL)
	for _, region := range []string{"europe", "america"} {
		f.stateDirs[region], f.uploadDirs[region] = t.TempDir(), t.TempDir()
		f.start(t, region)
	}
	t.Cleanup(func() {
		f.releaseOnce.Do(func() { close(f.releaseAck) })
		for _, app := range f.apps {
			app.stopReplication()
		}
		for _, server := range f.servers {
			server.Close()
		}
		for _, app := range f.apps {
			app.journal.Close()
			app.s.closeHistory()
		}
		f.primary.stopReplication()
		f.server.Close()
	})
	return f
}
func (f *replicationFixture) start(t *testing.T, region string) {
	t.Helper()
	t.Setenv("REGION", region)
	t.Setenv("REPLICA_STATE_DIR", f.stateDirs[region])
	t.Setenv("REGIONAL_STATE_DIR", f.uploadDirs[region])
	t.Setenv("IMAGE_CACHE_DIR", filepath.Join(f.primary.initial, "images"))
	h, app, err := serviceHandler()
	fixedRegion = "asia"
	if err != nil {
		t.Fatal(err)
	}
	f.apps[region], f.handlers[region] = app, h
	f.servers[region] = httptest.NewServer(h)
	t.Setenv("REGION_"+strings.ToUpper(region)+"_URL", f.servers[region].URL)
	awaitReplica(t, app)
}
func (f *replicationFixture) record(region, method, path, token string, body any) *httptest.ResponseRecorder {
	var data io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		data = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, data)
	r.Header.Set("X-Client-IP", "192.0.2.41")
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.handlers[region].ServeHTTP(w, r)
	return w
}
func (f *replicationFixture) request(t *testing.T, region, method, path, token string, body any, status int) object {
	t.Helper()
	w := f.record(region, method, path, token, body)
	if w.Code != status {
		t.Fatalf("%s %s at %s: %d %s", method, path, region, w.Code, w.Body.String())
	}
	var result object
	json.Unmarshal(w.Body.Bytes(), &result)
	return result
}
func (f *replicationFixture) user(t *testing.T, region, name string) (string, string) {
	t.Helper()
	registration := f.request(t, region, "POST", "/api/auth/register", "", object{"username": name, "displayName": name, "region": region}, 201)
	login := f.request(t, region, "POST", "/api/auth/login", "", object{"username": name, "secretKey": registration["secretKey"]}, 200)
	return login["user"].(map[string]any)["id"].(string), login["token"].(string)
}

func TestRegionalReadsReplicationAndRecovery(t *testing.T) {
	f := newReplicationFixture(t)
	uid, token := f.user(t, "europe", "regional_a")
	peer, peerToken := f.user(t, "europe", "regional_b")
	admin := f.request(t, "asia", "POST", "/api/admin/auth/login", "", object{"username": "admin", "password": "password"}, 200)["token"].(string)
	// Subscribe locally before the window exists; application of the replicated
	// notification must wake this stream without querying the primary.
	req, _ := http.NewRequest("GET", f.servers["europe"].URL+"/api/notifications", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("regional SSE did not start")
	}
	window := f.request(t, "asia", "POST", "/api/admin/posting-window", admin, object{"region": "europe", "localDate": "2026-11-01"}, 201)
	event, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Contains(event, []byte(window["id"].(string))) {
		t.Fatalf("regional SSE missed the window: %s %v", event, err)
	}
	post := upload(t, f.handlers["europe"], "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	peerPost := upload(t, f.handlers["europe"], "/api/posts", peerToken, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	relation := f.request(t, "europe", "POST", "/api/friend-requests", token, object{"recipientId": peer}, 201)
	f.request(t, "europe", "POST", "/api/friend-requests/"+relation["relationId"].(string)+"/accept", peerToken, nil, 200)
	f.request(t, "europe", "POST", "/api/posts/"+post["id"].(string)+"/comments", peerToken, object{"body": "replicated comment"}, 201)
	before := f.business.Load()
	for _, path := range []string{"/api/me", "/api/posting-window/current", "/api/posts/me/current", "/api/feeds/friends", "/api/posts/" + post["id"].(string) + "/comments", post["imageUrl"].(string), "/api/me/export"} {
		f.request(t, "europe", "GET", path, token, nil, 200)
	}
	if f.business.Load() != before {
		t.Fatal("regional reads queried the primary")
	}
	f.request(t, "america", "GET", post["imageUrl"].(string), "", nil, 200)
	f.request(t, "europe", "DELETE", "/api/posts/"+post["id"].(string), token, nil, 204)
	f.request(t, "america", "GET", post["imageUrl"].(string), "", nil, 403)
	// Restart from the replica's own durable log; no duplicate replay or reset.
	old := f.apps["europe"]
	epoch := old.replica.progress.Load().Epoch
	old.stopReplication()
	f.servers["europe"].Close()
	old.journal.Close()
	old.s.closeHistory()
	f.start(t, "europe")
	if f.apps["europe"].replica.progress.Load().Epoch != epoch || f.apps["europe"].replica.resets.Load() != 0 {
		t.Fatal("restart failed to resume its durable epoch")
	}
	me := f.request(t, "europe", "GET", "/api/me", token, nil, 200)
	if me["id"] != uid {
		t.Fatal("accepted account or session was lost")
	}
	for _, region := range []string{"asia", "europe", "america"} {
		f.request(t, region, "POST", replicationAckPath, "", object{"Epoch": epoch, "Seq": 999999, "Region": "america"}, 403)
	}
	invalid, _ := url.Parse(peerPost["imageUrl"].(string))
	query := invalid.Query()
	query.Set("sig", strings.Repeat("A", 43))
	invalid.RawQuery = query.Encode()
	for n := 0; n < 3; n++ {
		f.request(t, "europe", "GET", invalid.String(), "", nil, 403)
	}
	banned := f.request(t, "america", "GET", peerPost["imageUrl"].(string), "", nil, 403)
	if banned["error"] != "ip_banned" {
		t.Fatal("BAN did not reach another region")
	}
	status, _, _ := call(t, f.primary, "POST", "/api/initialize", "", nil)
	if status != 200 {
		t.Fatal("initialize did not reset all regions")
	}
	for _, region := range []string{"europe", "america"} {
		if f.apps[region].replica.progress.Load().Epoch == epoch {
			t.Fatal("initialize retained the old epoch")
		}
		f.request(t, region, "GET", post["imageUrl"].(string), "", nil, 403)
	}
	f.request(t, "europe", "GET", "/api/me", token, nil, 401)
}

func TestReplicationAckWaitDoesNotHoldStateLocks(t *testing.T) {
	f := newReplicationFixture(t)
	_, token := f.user(t, "asia", "queued_ack")
	f.blockAck.Store(true)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- f.record("asia", "PATCH", "/api/me", token, object{"bio": "first"}) }()
	waitBio := func(want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			got := f.request(t, "asia", "GET", "/api/me", token, nil, 200)
			if got["bio"] == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("state update did not execute while ACK was pending")
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitBio("first")
	select {
	case <-first:
		t.Fatal("success preceded all replica ACKs")
	default:
	}
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- f.record("asia", "PATCH", "/api/me", token, object{"bio": "second"}) }()
	waitBio("second")
	f.releaseOnce.Do(func() { close(f.releaseAck) })
	for _, done := range []chan *httptest.ResponseRecorder{first, second} {
		select {
		case w := <-done:
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
		case <-time.After(2 * time.Second):
			t.Fatal("ACK did not release successful updates")
		}
	}
	for _, region := range []string{"europe", "america"} {
		app := f.apps[region]
		app.mu.RLock()
		bio := app.s.Accounts.Get(f.primary.s.Sessions.Get(token)).Bio
		app.mu.RUnlock()
		if bio != "second" {
			t.Fatal("successful updates were not applied in order")
		}
	}
}
