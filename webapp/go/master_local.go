package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"
)

func ownedLocalRequest(r *http.Request) bool {
	if r.Method == "POST" && r.URL.Path == "/api/posts" {
		return true
	}
	if r.Method == "GET" {
		return r.URL.Path == "/api/me" || r.URL.Path == "/api/posting-window/current" || r.URL.Path == "/api/posts/me/current"
	}
	return (r.Method == "PATCH" || r.Method == "DELETE") && strings.HasPrefix(r.URL.Path, "/api/posts/") && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/api/posts/"), "/")
}

func (m *regionalMaster) loginHome(ctx context.Context, name string) (string, error) {
	m.mu.RLock()
	user := m.s.Accounts.Get(m.s.Names.Get(name))
	own := user != nil && user.HomeRegion == m.region
	cached := m.nameCache.Get(name)
	m.mu.RUnlock()
	if own {
		return m.region, nil
	}
	if home := m.homes.names[name]; home != "" {
		return home, nil
	}
	if cached != "" {
		return cached, nil
	}
	response, err := masterRequest(ctx, "asia", "home?username="+url.QueryEscape(name), nil)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", fmt.Errorf("home lookup: HTTP %d", response.StatusCode)
	}
	var value struct{ Home string }
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		return "", err
	}
	if value.Home != "" {
		m.mu.RLock()
		m.nameCache.Set(name, value.Home)
		m.mu.RUnlock()
	}
	return value.Home, nil
}

func (m *regionalMaster) localHTTP(w http.ResponseWriter, r *http.Request) {
	var prepared *preparedUpload
	if r.Method == "POST" && r.URL.Path == "/api/posts" {
		m.mu.RLock()
		check := httptest.NewRecorder()
		a := &App{appCore: m.signer.appCore, s: m.s}
		if time.Now().Before(m.s.Bans.Get(ipOf(r))) {
			failure(check, 403, "ip_banned")
		} else {
			a.auth(check, r, true)
		}
		m.mu.RUnlock()
		if check.Code != 200 {
			copyRecorded(w, check)
			return
		}
		r, prepared = m.files.prepare(m.stager, w, r)
		prepared.origin = m.region
		prepared.registrationUncertain = false
		r = r.WithContext(context.WithValue(r.Context(), uploadKey{}, prepared))
		defer m.files.discard(prepared)
	} else if r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if err != nil {
			failure(w, 413, "body_too_large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	read := r.Method == "GET"
	started := time.Now()
	if read {
		m.mu.RLock()
	} else {
		m.mu.Lock()
	}
	locked := time.Now()
	state := m.s
	if !read {
		state = m.s.fork()
	}
	a := &App{appCore: m.signer.appCore, s: state, prepared: !read, deferFailures: true}
	response := &deferredResponse{ResponseRecorder: httptest.NewRecorder(), ctx: r.Context()}
	if time.Now().Before(state.Bans.Get(ipOf(r))) {
		failure(response, 403, "ip_banned")
	} else {
		a.dispatch(response, r)
	}
	var err error
	if len(a.pending) > 0 {
		txn := masterTransaction{Epoch: m.epoch, ID: id(), Source: m.region, Sequence: m.sequence + 1, Changes: a.pending}
		err = m.commitLocked([]masterTransaction{txn})
	}
	if read {
		m.mu.RUnlock()
	} else {
		m.mu.Unlock()
	}
	kind := 2
	if r.URL.Path == "/api/auth/login" {
		kind = 0
	} else if !read {
		kind = 1
	}
	stats := &m.localMetrics[kind]
	stats.requests.Add(1)
	stats.waitNS.Add(uint64(locked.Sub(started)))
	stats.holdNS.Add(uint64(time.Since(locked)))
	if err != nil {
		if prepared != nil {
			prepared.committed = false
			prepared.registrationUncertain = true
		}
		failure(w, 500, "local_commit_failed")
		return
	}
	for _, kind := range a.failureEvents {
		if err := m.reportFailure(r, kind); err != nil {
			failure(w, 503, "failure_reporting_unavailable")
			return
		}
	}
	if prepared != nil && prepared.committed {
		m.files.finish(&http.Response{Request: r, Header: http.Header{regionalUploadAccepted: []string{prepared.fileID}}, StatusCode: response.Code})
	}
	// Local acknowledgements never wait for the coordinator or remote signer.
	// Image references retain their captured key and sign locally when encoded.
	if response.json && response.value != nil {
		json.NewEncoder(response.Body).Encode(response.value)
	}
	for key, values := range response.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.Code)
	if response.work != nil {
		response.work(w)
	} else {
		io.Copy(w, response.Body)
	}
}

func copyRecorded(w http.ResponseWriter, r *httptest.ResponseRecorder) {
	for key, values := range r.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(r.Code)
	io.Copy(w, r.Body)
}
