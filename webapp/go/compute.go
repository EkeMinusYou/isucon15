package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	measurement "github.com/EkeMinusYou/isucon-template/tools/measurement/app"
	"isuconapp/internal/jpegvalidate"
)

var computeClient = &http.Client{Transport: measurement.NewUpstreamTransport(), Timeout: 3 * time.Second}
var remoteJPEG atomic.Uint64

type signatureJob struct {
	ID, Secret string
	Exp        int64
}

// The compute host has no application state. Only authenticated peers may
// delegate validation or signatures; request bytes and signature rules stay
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
		case "/api/internal/compute/jpeg":
			if err := jpegvalidate.Validate(r.Body); err != nil {
				failure(w, 400, "invalid_jpeg")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/internal/compute/signatures":
			var jobs []signatureJob
			if !decode(r, &jobs) || len(jobs) > 256 {
				failure(w, 400, "invalid_jobs")
				return
			}
			results := make([]string, len(jobs))
			for i, j := range jobs {
				results[i] = a.signImage(j.ID, j.Secret, j.Exp)
			}
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
func validateJPEG(ctx context.Context, b []byte) bool {
	if os.Getenv("COMPUTE_URL") != "" {
		resp, err := computeRequest(ctx, "jpeg", b)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 204 {
				remoteJPEG.Add(1)
				return true
			}
			if resp.StatusCode == 400 {
				remoteJPEG.Add(1)
				return false
			}
		}
	}
	return jpegvalidate.Validate(bytes.NewReader(b)) == nil
}
func signatureKey(id string, exp int64) string { return id + ":" + strconv.FormatInt(exp, 10) }
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
	jobs := make([]signatureJob, 0, len(refs))
	seen := map[string]bool{}
	a.signatureMu.Lock()
	for _, r := range refs {
		key := signatureKey(r.id, r.exp)
		if a.signatures[key] == "" && !seen[key] {
			jobs = append(jobs, signatureJob{r.id, r.secret, r.exp})
			seen[key] = true
		}
	}
	a.signatureMu.Unlock()
	for start := 0; start < len(jobs); start += 256 {
		end := start + 256
		if end > len(jobs) {
			end = len(jobs)
		}
		batch := jobs[start:end]
		b, err := json.Marshal(batch)
		if err != nil {
			return
		}
		resp, err := computeRequest(ctx, "signatures", b)
		if err != nil {
			return
		}
		var results []string
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&results)
		resp.Body.Close()
		if resp.StatusCode != 200 || err != nil || len(results) != len(batch) {
			return
		}
		a.signatureMu.Lock()
		if len(a.signatures)+len(batch) > 65536 {
			a.signatures = map[string]string{}
			a.signatureResets.Add(1)
		}
		for i, v := range batch {
			a.signatures[signatureKey(v.ID, v.Exp)] = results[i]
		}
		a.signatureMu.Unlock()
		a.remoteSignatures.Add(uint64(len(batch)))
	}
}
