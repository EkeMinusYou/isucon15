package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

type stalledResponse struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *stalledResponse) Write(data []byte) (int, error) {
	select {
	case <-w.entered:
	default:
		close(w.entered)
	}
	<-w.release
	return w.ResponseRecorder.Write(data)
}

func TestStateExecutorDoesNotWaitForImageOrExportTransfer(t *testing.T) {
	a := fixtureApp(t)
	_, token := register(t, a, "executor_stream")
	openWindow(t, a, "2026-11-01")
	post := upload(t, a, "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	for _, path := range []string{post["imageUrl"].(string), "/api/me/export"} {
		t.Run(path, func(t *testing.T) {
			w := &stalledResponse{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
			done := make(chan struct{})
			go func() {
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				a.ServeHTTP(w, r)
				close(done)
			}()
			defer func() { close(w.release); <-done }()
			select {
			case <-w.entered:
			case <-time.After(time.Second):
				t.Fatal("transfer did not start")
			}
			updated := make(chan int, 1)
			go func() {
				status, _, _ := call(t, a, "PATCH", "/api/me", token, object{"bio": "updated during transfer"})
				updated <- status
			}()
			select {
			case status := <-updated:
				if status != 200 {
					t.Fatalf("profile update: %d", status)
				}
			case <-time.After(time.Second):
				t.Fatal("a slow transfer blocked state execution")
			}
		})
	}
}
