package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func assertSignal(t *testing.T, ch <-chan struct{}, closed bool) {
	t.Helper()
	select {
	case <-ch:
		if !closed {
			t.Fatal("unrelated subscription was notified")
		}
	default:
		if closed {
			t.Fatal("subscription missed its notification")
		}
	}
}
func TestNotificationTargetsAndCleanup(t *testing.T) {
	var hub notificationHub
	a := hub.subscribe("alice", "asia")
	b := hub.subscribe("bob", "asia")
	c := hub.subscribe("carol", "europe")
	au, ar := a.channels()
	bu, br := b.channels()
	cu, cr := c.channels()
	hub.userChanged("alice")
	assertSignal(t, au, true)
	for _, ch := range []<-chan struct{}{ar, bu, br, cu, cr} {
		assertSignal(t, ch, false)
	}
	hub.regionChanged("asia")
	assertSignal(t, ar, true)
	assertSignal(t, br, true)
	assertSignal(t, cr, false)
	au, ar = a.channels()
	hub.reset()
	assertSignal(t, au, true)
	assertSignal(t, ar, true)
	a.close()
	b.close()
	c.close()
	if len(hub.users) != 0 || len(hub.regions) != 0 {
		t.Fatal("subscriptions leaked")
	}
}
func TestSSEClosesOnRegionChange(t *testing.T) {
	a := fixtureApp(t)
	_, token := register(t, a, "moving_listener")
	server := httptest.NewServer(a)
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+"/api/notifications", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	requireCall(t, a, "PATCH", "/api/me", token, object{"region": "europe"}, 200)
	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) != 0 {
		t.Fatalf("region change: body=%q error=%v", body, err)
	}
}
