package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImagePermitRevocationRejectsDelayedGenerations(t *testing.T) {
	oldSecret := transportSecret
	transportSecret = "permit-test"
	t.Cleanup(func() { transportSecret = oldSecret })
	owner := newOwnerImagePermits()
	wire := imagePermit{URL: "/api/images/example?exp=9999999999&sig=valid", Part: "0000.bin", Size: 3, Exp: 9999999999, MediaType: "image/jpeg", Post: true}
	ip := "192.0.2.1"
	first, _ := owner.grant("europe", wire, []string{"ip:" + ip})
	second, _ := owner.grant("europe", wire, []string{"ip:" + ip})
	retired := owner.revoke("ip:" + ip)
	if len(retired) != 2 || first.ID == second.ID {
		t.Fatal("revocation did not retain every live grant generation")
	}
	region := newRegionalImagePermits()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, wire.Part), []byte("jpg"), 0600)
	request := httptest.NewRequest("GET", wire.URL, nil)
	request.Header.Set("X-Client-IP", ip)
	request = withRegionRequestStart(request)
	install := func(grant imagePermit) {
		b, _ := json.Marshal([]imagePermit{grant})
		response := &http.Response{Request: request, Header: http.Header{}}
		response.Header.Set(imagePermitHeader, base64.RawURLEncoding.EncodeToString(b))
		region.install(response)
		if response.Header.Get(imagePermitHeader) != "" {
			t.Fatal("internal grants leaked")
		}
	}
	install(second)
	if !region.serve(httptest.NewRecorder(), request, root, nil, &regionalImageMetrics{}) {
		t.Fatal("valid installed permission was not used")
	}
	localKey := localPermitKey{ip, wire.URL}
	grant := region.entries[localKey]
	grant.deadline = time.Now().Add(1500 * time.Millisecond)
	region.entries[localKey] = grant
	w := httptest.NewRecorder()
	region.serve(w, request, root, nil, &regionalImageMetrics{})
	if !strings.Contains(w.Header().Get("Cache-Control"), "max-age=1,") {
		t.Fatal("client cache lifetime extended beyond the serving permission")
	}
	ids, _ := json.Marshal([]string{first.ID, second.ID})
	revoke := httptest.NewRequest("POST", imageRevokePath, bytes.NewReader(ids))
	revoke.Header.Set("X-ISU-Transport", transportSecret)
	ack := httptest.NewRecorder()
	region.revoke(ack, revoke)
	if ack.Code != 204 {
		t.Fatal("revocation did not acknowledge")
	}
	install(first)
	if region.serve(httptest.NewRecorder(), request, root, nil, &regionalImageMetrics{}) {
		t.Fatal("a delayed old generation revived a revoked permission")
	}
	third, _ := owner.grant("europe", wire, []string{"ip:" + ip})
	install(third)
	region.reset()
	install(third)
	if region.serve(httptest.NewRecorder(), request, root, nil, &regionalImageMetrics{}) {
		t.Fatal("a pre-initialize response restored a permission after reset")
	}
}

func TestImagePermitRevocationDoesNotDelayUpdate(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release; w.WriteHeader(503) }))
	defer server.Close()
	defer close(release)
	t.Setenv("REGION_EUROPE_URL", server.URL)
	a := &App{appCore: &appCore{imagePermits: newOwnerImagePermits()}}
	deadline := time.Now().Add(5 * time.Second)
	a.revocations = []*heldImagePermit{{imagePermit: imagePermit{ID: "test"}, region: "europe", deadline: deadline}}
	returned := make(chan struct{})
	go func() { a.sendImageRevocations(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("update waited for a regional acknowledgement")
	}
}
