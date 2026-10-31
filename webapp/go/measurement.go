package main

import (
	measurement "github.com/EkeMinusYou/isucon-template/tools/measurement/app"
	"net/http"
	"strings"
	"sync/atomic"
)

// Bootstrap calls this after identifying the contest's session. No cookie name
// or authentication scheme is built into the generic application skeleton.
func setMeasurementIdentity(r *http.Request, sessionIdentity string) {
	measurement.SetSessionIdentity(r, sessionIdentity)
}

// connectionPolicy preserves long-lived HTTP/1 connections up to the source
// recipe's finite request cap. HTTP/2 lifecycle remains owned by net/http.
func connectionPolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 1 && measurement.ConnectionRequestCount(r) >= 1000000 {
			r.Close = true
			w.Header().Set("Connection", "close")
		}
		next.ServeHTTP(w, r)
	})
}

// Counters cover the complete request phases and are sampled by the standard
// runtime collector. No state lock is taken by the measurement endpoint.
type requestMetrics struct {
	requests   atomic.Uint64
	waitNS     atomic.Uint64
	holdNS     atomic.Uint64
	responseNS atomic.Uint64
}

var routeMetricNames = [...]string{"post", "image", "feed", "memory", "export", "auth", "other"}

func metricRoute(path string) int {
	switch {
	case path == "/api/posts":
		return 0
	case strings.HasPrefix(path, "/api/images/"):
		return 1
	case strings.HasPrefix(path, "/api/feeds/"):
		return 2
	case strings.HasPrefix(path, "/api/memories"):
		return 3
	case path == "/api/me/export":
		return 4
	case strings.HasPrefix(path, "/api/auth/"):
		return 5
	default:
		return 6
	}
}
func (a *App) snapshot() measurement.Snapshot {
	m := map[string]any{
		"app_sig_hits_total":       a.signatureHits.Load(),
		"app_sig_misses_total":     a.signatureMisses.Load(),
		"app_sig_resets_total":     a.signatureResets.Load(),
		"app_sig_ns_total":         a.signatureNS.Load(),
		"app_sig_coalesced_total":  a.signatureCoalesced.Load(),
		"app_sig_evicted_total":    a.signatureEvictions.Load(),
		"app_sig_rpc_failed_total": a.signatureRPCFailures.Load(),
		"app_remote_sig_total":     a.remoteSignatures.Load(),
		"app_remote_jpeg_total":    remoteJPEG.Load(),
	}
	for i, name := range routeMetricNames {
		p := "app_" + name + "_"
		s := &a.routeMetrics[i]
		m[p+"requests_total"] = s.requests.Load()
		m[p+"wait_ns_total"] = s.waitNS.Load()
		m[p+"hold_ns_total"] = s.holdNS.Load()
		m[p+"response_ns_total"] = s.responseNS.Load()
	}
	return measurement.Snapshot{Metrics: m}
}
