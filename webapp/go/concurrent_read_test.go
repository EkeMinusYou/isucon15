package main

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestConcurrentReadsAndProfileUpdates(t *testing.T) {
	a := fixtureApp(t)
	_, token := register(t, a, "concurrent_reads")
	admin := requireCall(t, a, "POST", "/api/admin/auth/login", "", object{"username": "admin", "password": "password"}, 200)["token"].(string)
	requireCall(t, a, "POST", "/api/admin/posting-window", admin, object{"region": "asia", "localDate": "2026-11-01"}, 201)
	upload(t, a, "/api/posts", token, map[string]string{"caption": "concurrent", "visibility": "public", "location_kind": "none"}, 201)
	var wg sync.WaitGroup
	errors := make(chan error, 9)
	start := make(chan struct{})
	for n := 0; n < 9; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			for i := 0; i < 25; i++ {
				method, path := "GET", []string{"/api/me", "/api/posting-window/current", "/api/feeds/public"}[i%3]
				var body []byte
				if n == 0 {
					method, path, body = "PATCH", "/api/me", []byte(`{"displayName":"updated"}`)
				}
				r := httptest.NewRequest(method, path, bytes.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				a.ServeHTTP(w, r)
				if w.Code != 200 {
					errors <- fmt.Errorf("%s %s: status %d", method, path, w.Code)
					return
				}
			}
		}(n)
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}
