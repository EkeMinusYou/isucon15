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
	responseNS atomic.Uint64 // Streaming responses include transfer time; JSON records materialization.
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
	entropy := 0
	if validateJPEGEntropy() {
		entropy = 1
	}
	m := map[string]any{
		"app_notification_wakeups_total": a.notifier.wakeups.Load(),
		"app_notification_targets_total": a.notifier.targeted.Load(),
		"app_transaction_requests_total": a.transactionRequests.Load(),
		"app_transaction_shards_total":   a.transactionShards.Load(),
		"app_transaction_global_total":   a.transactionGlobal.Load(),
		"app_stripe_retries_total":       a.stripeRetries.Load(),
		"app_journal_wait_ns_total":      a.journalWaitNS.Load(),
		"app_journal_write_ns_total":     a.journalWriteNS.Load(),
		"app_upload_stage_ns_total":      a.uploadStageNS.Load(),
		"app_upload_stage_bytes_total":   a.uploadStageBytes.Load(),
		"app_history_resident_bytes":     a.historyResidentBytes.Load(),
		"app_jpeg_entropy_validation":    entropy,
		"app_jpeg_queue_depth":           jpegQueued.Load(),
		"app_jpeg_active":                jpegActive.Load(),
		"app_jpeg_requests_total":        jpegRequests.Load(),
		"app_jpeg_cancelled_total":       jpegCancelled.Load(),
		"app_jpeg_wait_ns_total":         jpegWaitNS.Load(),
		"app_jpeg_work_ns_total":         jpegWorkNS.Load(),
		"app_jpeg_remote_requests_total": jpegRemoteRequests.Load(),
		"app_jpeg_remote_failures_total": jpegRemoteFailures.Load(),
		"app_crypto_arch":                nativeCryptoArch.Load(),
		"app_native_sig_jobs_total":      nativeSignatureJobs.Load(),
		"app_scalar_sig_jobs_total":      scalarSignatureJobs.Load(),
		"app_sig_hits_total":             a.signatureHits.Load(),
		"app_sig_misses_total":           a.signatureMisses.Load(),
		"app_sig_resets_total":           a.signatureResets.Load(),
		"app_sig_ns_total":               a.signatureNS.Load(),
		"app_sig_coalesced_total":        a.signatureCoalesced.Load(),
		"app_sig_evicted_total":          a.signatureEvictions.Load(),
		"app_sig_rpc_failed_total":       a.signatureRPCFailures.Load(),
		"app_remote_sig_total":           a.remoteSignatures.Load(),
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
