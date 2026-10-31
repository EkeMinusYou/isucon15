package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	measurement "github.com/EkeMinusYou/isucon-template/tools/measurement/app"
)

var computeClient = &http.Client{Transport: measurement.NewUpstreamTransport(), Timeout: 3 * time.Second}
var nativeSignatureBatch func([]signatureJob) ([]string, bool)
var nativeCryptoArch atomic.Int64
var nativeSignatureJobs atomic.Uint64
var scalarSignatureJobs atomic.Uint64

type signatureJob struct {
	ID, Secret string
	Exp        int64
}

// The compute host has no application state. Only authenticated peers may
// delegate signatures; request bytes and signature rules stay
// identical to local execution.
func computeHandler(a *App) http.Handler {
	slots := make(chan struct{}, 2)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
			return
		}
		if !isInternal(r) {
			failure(w, 403, "forbidden")
			return
		}
		if r.Method != "POST" {
			failure(w, 405, "method_not_allowed")
			return
		}
		select {
		case slots <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		defer func() { <-slots }()
		r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
		switch r.URL.Path {
		case "/api/internal/compute/signatures":
			// Keep the worker stateless; only the owner's resettable cache holds results.
			var jobs []signatureJob
			if !decode(r, &jobs) || len(jobs) > 256 {
				failure(w, 400, "invalid_jobs")
				return
			}

			started := time.Now()
			var results []string
			ok := false
			if nativeSignatureBatch != nil && len(jobs) >= 8 {
				results, ok = nativeSignatureBatch(jobs)
			}
			if ok {
				nativeSignatureJobs.Add(uint64(len(jobs)))
			} else {
				results = make([]string, len(jobs))
				for i, j := range jobs {
					results[i] = computeImageSignature(j.ID, j.Secret, j.Exp)
				}
				scalarSignatureJobs.Add(uint64(len(jobs)))
			}
			a.signatureMisses.Add(uint64(len(jobs)))
			a.signatureNS.Add(uint64(time.Since(started)))
			output(w, 200, results)
		default:
			failure(w, 404, "not_found")
		}
	})
}
func computeRequest(ctx context.Context, path string, body []byte) (*http.Response, error) {
	url := strings.TrimRight(os.Getenv("COMPUTE_URL"), "/") + "/api/internal/compute/" + path
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-ISU-Transport", transportSecret)
	req.Header.Set("X-ISU-Region", "asia")
	return computeClient.Do(req)
}
func prefetchImageSignatures(ctx context.Context, value any) {
	var refs []imageReference
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case imageReference:
			refs = append(refs, x)
		case map[string]any:
			for _, v := range x {
				visit(v)
			}
		case []object:
			for _, v := range x {
				visit(v)
			}
		case []any:
			for _, v := range x {
				visit(v)
			}
		}
	}
	visit(value)
	if len(refs) == 0 || os.Getenv("COMPUTE_URL") == "" {
		return
	}

	a := refs[0].app
	type pending struct {
		job    signatureJob
		key    string
		flight *signatureFlight
	}
	jobs := []pending{}
	waiting := []*signatureFlight{}
	seen := map[string]bool{}
	for _, r := range refs {
		key := signatureKey(r.id, r.secret, r.exp)
		if seen[key] {
			continue
		}
		seen[key] = true
		value, f, owner := a.reserveSignature(key)
		if value != "" {
			continue
		}
		if !owner {
			waiting = append(waiting, f)
			continue
		}
		jobs = append(jobs, pending{signatureJob{r.id, r.secret, r.exp}, key, f})
	}
	for start := 0; start < len(jobs); start += 256 {
		end := start + 256
		if end > len(jobs) {
			end = len(jobs)
		}
		batch := make([]signatureJob, end-start)
		for i, p := range jobs[start:end] {
			batch[i] = p.job
		}
		b, _ := json.Marshal(batch)
		resp, err := computeRequest(ctx, "signatures", b)
		var results []string
		if err == nil {
			err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&results)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				results = nil
			}
		}
		valid := err == nil && len(results) == len(batch)
		if valid {
			a.remoteSignatures.Add(uint64(len(batch)))
		} else {
			a.signatureRPCFailures.Add(1)
		}
		for i, p := range jobs[start:end] {
			value := ""
			if valid {
				value = results[i]
			} else {
				before := time.Now()
				value = computeImageSignature(p.job.ID, p.job.Secret, p.job.Exp)
				a.signatureMisses.Add(1)
				a.signatureNS.Add(uint64(time.Since(before)))
			}
			a.finishSignature(p.key, p.flight, value)
		}
	}
	for _, f := range waiting {
		<-f.done
	}
}
