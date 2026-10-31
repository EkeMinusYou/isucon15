package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQueuedImageFailuresDoNotExtendActiveBan(t *testing.T) {
	previous := transportSecret
	transportSecret = "queued-image-failures"
	t.Cleanup(func() { transportSecret = previous })
	for _, bannedCaller := range []bool{false, true} {
		t.Run(map[bool]string{false: "target", true: "target-and-caller"}[bannedCaller], func(t *testing.T) {
			a := fixtureApp(t)
			ip := "198.51.100.9"
			until := time.Now().Add(time.Minute).UTC()
			a.put("ban", banChange{ip, until})
			if bannedCaller {
				a.put("ban", banChange{"192.0.2.1", until})
			}
			a.commit()
			body, _ := json.Marshal(object{"ip": ip, "region": "asia", "kind": "signature"})
			for i := 0; i < 4; i++ {
				r := httptest.NewRequest("POST", masterPrefix+"failure", bytes.NewReader(body))
				r.Header.Set("X-ISU-Transport", transportSecret)
				w := httptest.NewRecorder()
				a.ServeHTTP(w, r)
				if w.Code != 204 {
					t.Fatalf("queued failure: %d %s", w.Code, w.Body.String())
				}
			}
			if !a.s.Bans.Get(ip).Equal(until) || a.failures.Len() != 0 {
				t.Fatal("queued failures changed an active ban")
			}
		})
	}
}
