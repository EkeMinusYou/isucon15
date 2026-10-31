package main

import (
	"testing"
	"time"
)

func TestImageExpiryReuseAndLifetime(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	exp := imageExpiry(start)
	for _, offset := range []time.Duration{0, time.Second, 59*time.Minute + 59*time.Second} {
		now := start.Add(offset)
		if imageExpiry(now) != exp {
			t.Fatal("same-hour URLs cannot reuse their signature")
		}
		remaining := time.Unix(exp, 0).Sub(now)
		if remaining < 24*time.Hour || remaining > 25*time.Hour {
			t.Fatalf("unexpected lifetime: %v", remaining)
		}
	}
	if imageExpiry(start.Add(time.Hour)) != exp+3600 {
		t.Fatal("expiry did not advance at the boundary")
	}
}
