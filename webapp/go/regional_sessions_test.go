package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func sessionTestUser(t *testing.T, f *replicationFixture, region, name string) (string, string, string) {
	t.Helper()
	registration := f.request(t, region, "POST", "/api/auth/register", "", object{"username": name, "displayName": name, "region": region}, 201)
	secret := registration["secretKey"].(string)
	login := f.request(t, region, "POST", "/api/auth/login", "", object{"username": name, "secretKey": secret}, 200)
	return login["user"].(map[string]any)["id"].(string), secret, login["token"].(string)
}
func replicationHead(f *replicationFixture) int {
	f.primary.replication.mu.Lock()
	defer f.primary.replication.mu.Unlock()
	return len(f.primary.replication.entries)
}

func TestRegionalSessionsRevocationAndRecovery(t *testing.T) {
	f := newReplicationFixture(t, true)
	uid, secret, token := sessionTestUser(t, f, "europe", "session_eu")
	head := replicationHead(f)
	login := f.request(t, "europe", "POST", "/api/auth/login", "", object{"username": "session_eu", "secretKey": secret}, 200)
	next := login["token"].(string)
	f.request(t, "europe", "GET", "/api/me", token, nil, 401)
	f.request(t, "europe", "GET", "/api/me", next, nil, 200)
	results := make(chan *httptest.ResponseRecorder, 2)
	for n := 0; n < 2; n++ {
		go func() {
			results <- f.record("europe", "POST", "/api/auth/login", "", object{"username": "session_eu", "secretKey": secret})
		}()
	}
	var tokens []string
	for n := 0; n < 2; n++ {
		response := <-results
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		var value object
		json.Unmarshal(response.Body.Bytes(), &value)
		tokens = append(tokens, value["token"].(string))
	}
	active := 0
	for _, token := range tokens {
		response := f.record("europe", "GET", "/api/me", token, nil)
		if response.Code == 200 {
			active++
			next = token
		} else if response.Code != 401 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if active != 1 {
		t.Fatal("concurrent logins did not replace exactly one session", active)
	}
	if replicationHead(f) != head {
		t.Fatal("successful login entered public replication")
	}
	for _, region := range []string{"asia", "europe", "america"} {
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.Header.Set(authenticatedUserHeader, uid)
		r.Header.Set("X-ISU-Transport", "forged")
		w := httptest.NewRecorder()
		f.handlers[region].ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("public identity header bypassed authentication", region, w.Code)
		}
		f.request(t, region, "POST", sessionInternalPrefix+"login", "", nil, 403)
	}
	f.request(t, "europe", "PATCH", "/api/me", next, object{"region": "america"}, 200)
	f.request(t, "europe", "GET", "/api/me", next, nil, 403)
	f.request(t, "america", "GET", "/api/me", next, nil, 200)
	head = replicationHead(f)
	login = f.request(t, "america", "POST", "/api/auth/login", "", object{"username": "session_eu", "secretKey": secret}, 200)
	next = login["token"].(string)
	if tokenRegion(next) != "europe" {
		t.Fatal("region move changed the authentication owner")
	}
	f.request(t, "asia", "POST", "/api/auth/logout", next, nil, 204)
	f.request(t, "america", "GET", "/api/me", next, nil, 401)
	if replicationHead(f) != head || len(f.primary.s.Sessions.Snapshot()) != 0 {
		t.Fatal("session state leaked into public state")
	}
	// No ACK is available, but successful authentication is still local.
	f.blockAck.Store(true)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- f.record("america", "POST", "/api/auth/login", "", object{"username": "session_eu", "secretKey": secret})
	}()
	select {
	case response := <-done:
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		var result object
		json.Unmarshal(response.Body.Bytes(), &result)
		next = result["token"].(string)
	case <-time.After(time.Second):
		t.Fatal("login waited for public-state ACKs")
	}
	if replicationHead(f) != head {
		t.Fatal("login appended a public-state transaction")
	}
	f.releaseOnce.Do(func() { close(f.releaseAck) })
	old := f.apps["europe"]
	old.stopReplication()
	f.servers["europe"].Close()
	old.journal.Close()
	old.sessions.file.Close()
	old.s.closeHistory()
	f.start(t, "europe")
	f.request(t, "america", "GET", "/api/me", next, nil, 200)
	f.request(t, "america", "GET", "/api/me", token, nil, 401)
	f.request(t, "america", "POST", "/api/me/account-deletion", next, nil, 204)
	f.request(t, "america", "GET", "/api/me", next, nil, 401)
	r := httptest.NewRequest("PATCH", "/api/me", strings.NewReader(`{"bio":"stale command"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-ISU-Transport", transportSecret)
	r.Header.Set("X-ISU-Region", "america")
	r.Header.Set(authenticatedUserHeader, uid)
	w := httptest.NewRecorder()
	f.primary.ownerHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("writer accepted an authenticated command for a deleted account", w.Code)
	}
	f.request(t, "america", "POST", "/api/auth/login", "", object{"username": "session_eu", "secretKey": secret}, 401)
	f.request(t, "asia", "POST", "/api/initialize", "", nil, 200)
	for _, app := range []*App{f.primary, f.apps["europe"], f.apps["america"]} {
		app.sessions.mu.RLock()
		count := len(app.sessions.tokens)
		app.sessions.mu.RUnlock()
		if count != 0 {
			t.Fatal("initialize retained regional tokens")
		}
	}
}

func TestRegionalSessionFailuresStayAtOriginalIngress(t *testing.T) {
	f := newReplicationFixture(t, true)
	_, euSecret, token := sessionTestUser(t, f, "europe", "fail_eu")
	_, _, _ = sessionTestUser(t, f, "america", "fail_us")
	admin := f.request(t, "asia", "POST", "/api/admin/auth/login", "", object{"username": "admin", "password": "password"}, 200)["token"].(string)
	f.request(t, "asia", "POST", "/api/admin/posting-window", admin, object{"region": "europe", "localDate": "2026-11-01"}, 201)
	post := upload(t, f.handlers["europe"], "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	for n := 0; n < 9; n++ {
		name := "fail_eu"
		if n%2 != 0 {
			name = "fail_us"
		}
		f.request(t, "europe", "POST", "/api/auth/login", "", object{"username": name, "secretKey": "wrong"}, 401)
	}
	login := f.request(t, "europe", "POST", "/api/auth/login", "", object{"username": "fail_eu", "secretKey": euSecret}, 200)
	token = login["token"].(string)
	f.request(t, "america", "POST", "/api/auth/login", "", object{"username": "fail_eu", "secretKey": "wrong"}, 401)
	f.request(t, "europe", "GET", "/api/me", token, nil, 200)
	f.request(t, "europe", "POST", "/api/auth/login", "", object{"username": "fail_us", "secretKey": "wrong"}, 401)
	for _, region := range []string{"asia", "europe", "america"} {
		body := f.request(t, region, "GET", "/api/me", token, nil, 403)
		if body["error"] != "ip_banned" {
			t.Fatal("threshold response preceded global BAN", region, body)
		}
	}
	if f.apps["europe"].sessions.failed.Load() != 10 || f.apps["america"].sessions.failed.Load() != 1 {
		t.Fatal("failures followed authentication owners or successful login reset the count")
	}
	invalid, _ := url.Parse(post["imageUrl"].(string))
	query := invalid.Query()
	query.Set("sig", strings.Repeat("A", 43))
	invalid.RawQuery = query.Encode()
	badJPEG := func(ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/me/profile-image", strings.NewReader("invalid jpeg"))
		r.Header.Set("Content-Type", "image/jpeg")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Client-IP", ip)
		w := httptest.NewRecorder()
		f.handlers["europe"].ServeHTTP(w, r)
		return w
	}
	for _, test := range []struct{ ip, kind string }{{"192.0.2.42", "jpeg"}, {"192.0.2.43", "signature"}} {
		if badJPEG(test.ip).Code != 400 {
			t.Fatal("invalid JPEG did not retain its response")
		}
		for n := 0; n < 2; n++ {
			w := f.recordIP("europe", "GET", invalid.String(), "", nil, test.ip)
			if w.Code != 403 {
				t.Fatal("invalid signature response", w.Code)
			}
		}
		if w := f.recordIP("europe", "GET", post["imageUrl"].(string), "", nil, test.ip); w.Code != 200 {
			t.Fatal("different failure kinds were combined", w.Code, w.Body.String())
		}
		var response *httptest.ResponseRecorder
		if test.kind == "jpeg" {
			response = badJPEG(test.ip)
		} else {
			f.blockAck.Store(true)
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() { result <- f.recordIP("europe", "GET", invalid.String(), "", nil, test.ip) }()
			select {
			case <-result:
				t.Fatal("BAN threshold completed without ACKs")
			case <-time.After(30 * time.Millisecond):
			}
			f.releaseOnce.Do(func() { close(f.releaseAck) })
			select {
			case response = <-result:
			case <-time.After(time.Second):
				t.Fatal("BAN did not finish after ACKs")
			}
		}
		if response.Code != 400 && response.Code != 403 {
			t.Fatal("threshold response", response.Code)
		}
		for _, region := range []string{"asia", "europe", "america"} {
			w := f.recordIP(region, "GET", post["imageUrl"].(string), "", nil, test.ip)
			if w.Code != 403 || !strings.Contains(w.Body.String(), "ip_banned") {
				t.Fatal("BAN was not shared", region, w.Code, w.Body.String())
			}
		}
	}
}
