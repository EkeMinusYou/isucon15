package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type regionRequestStart struct{}
type localPermitKey struct{ ip, url string }
type localImagePermit struct {
	imagePermit
	deadline time.Time
}

type regionalImagePermits struct {
	mu        sync.Mutex
	entries   map[localPermitKey]localImagePermit
	byID      map[string]localPermitKey
	revoked   map[string]time.Time
	notBefore time.Time
	sweepAt   time.Time
	hits      atomic.Uint64
	misses    atomic.Uint64
}

func newRegionalImagePermits() *regionalImagePermits {
	return &regionalImagePermits{entries: map[localPermitKey]localImagePermit{}, byID: map[string]localPermitKey{}, revoked: map[string]time.Time{}, notBefore: time.Now()}
}

func (p *regionalImagePermits) sweep(now time.Time) {
	if now.Before(p.sweepAt) {
		return
	}
	p.sweepAt = now.Add(time.Second)
	for key, entry := range p.entries {
		if !now.Before(entry.deadline) {
			delete(p.entries, key)
			delete(p.byID, entry.ID)
		}
	}
	for id, until := range p.revoked {
		if !now.Before(until) {
			delete(p.revoked, id)
		}
	}
}

func (p *regionalImagePermits) install(resp *http.Response) {
	headers := resp.Header.Values(imagePermitHeader)
	resp.Header.Del(imagePermitHeader)
	start, ok := resp.Request.Context().Value(regionRequestStart{}).(time.Time)
	if !ok || len(headers) == 0 || !time.Now().Before(start.Add(imagePermitLifetime)) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if start.Before(p.notBefore) {
		return
	}
	now := time.Now()
	p.sweep(now)
	for _, header := range headers {
		if len(header) > 256<<10 {
			continue
		}
		data, err := base64.RawURLEncoding.DecodeString(header)
		var grants []imagePermit
		if err != nil || json.Unmarshal(data, &grants) != nil || len(grants) > 128 {
			continue
		}
		for _, grant := range grants {
			deadline := start.Add(imagePermitLifetime)
			if expiry := time.Unix(grant.Exp, 0); expiry.Before(deadline) {
				deadline = expiry
			}
			if grant.ID == "" || grant.Part == "" || grant.Part != filepath.Base(grant.Part) || grant.Offset < 0 || grant.Size < 0 || !strings.HasPrefix(grant.URL, "/api/images/") || !now.Before(deadline) || now.Before(p.revoked[grant.ID]) {
				continue
			}
			key := localPermitKey{ipOf(resp.Request), grant.URL}
			if previous, ok := p.entries[key]; ok {
				if previous.deadline.After(deadline) {
					continue
				}
				delete(p.byID, previous.ID)
			} else if len(p.entries) >= 32768 {
				continue
			}
			p.entries[key] = localImagePermit{grant, deadline}
			p.byID[grant.ID] = key
		}
	}
}

func (p *regionalImagePermits) revoke(w http.ResponseWriter, r *http.Request) {
	if !isInternal(r) {
		failure(w, 403, "forbidden")
		return
	}
	if r.Method != "POST" {
		failure(w, 405, "method_not_allowed")
		return
	}
	var ids []string
	if json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&ids) != nil || len(ids) > 65536 {
		failure(w, 400, "invalid_revocation")
		return
	}
	p.mu.Lock()
	now := time.Now()
	p.sweep(now)
	for _, id := range ids {
		p.revoked[id] = now.Add(imagePermitLifetime)
		if key, ok := p.byID[id]; ok {
			if p.entries[key].ID == id {
				delete(p.entries, key)
			}
			delete(p.byID, id)
		}
	}
	p.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (p *regionalImagePermits) reset() {
	p.mu.Lock()
	p.entries = map[localPermitKey]localImagePermit{}
	p.byID = map[string]localPermitKey{}
	p.revoked = map[string]time.Time{}
	p.notBefore = time.Now()
	p.mu.Unlock()
}

func (p *regionalImagePermits) serve(w http.ResponseWriter, r *http.Request, root string, uploads *regionalUploads, stats *regionalImageMetrics) bool {
	if r.Method != "GET" || !strings.HasPrefix(r.URL.Path, "/api/images/") {
		return false
	}
	key := localPermitKey{ipOf(r), r.URL.RequestURI()}
	p.mu.Lock()
	grant, ok := p.entries[key]
	if ok && (!time.Now().Before(grant.deadline) || grant.Exp <= time.Now().Unix()) {
		delete(p.entries, key)
		delete(p.byID, grant.ID)
		ok = false
	}
	p.mu.Unlock()
	if !ok {
		p.misses.Add(1)
		return false
	}
	var f *os.File
	var err error
	if grant.Uploaded && uploads != nil {
		f, err = uploads.open(grant.Part)
	} else if !grant.Uploaded {
		f, err = os.Open(filepath.Join(root, grant.Part))
	} else {
		err = os.ErrNotExist
	}
	if err == nil {
		info, statErr := f.Stat()
		if statErr != nil || grant.Offset > info.Size() || grant.Size > info.Size()-grant.Offset {
			f.Close()
			err = os.ErrNotExist
		}
	}
	if err != nil {
		p.misses.Add(1)
		return false
	}
	defer f.Close()
	// Opening the original may wait. Do not publish a grant which expired or
	// was revoked during that wait.
	p.mu.Lock()
	current, present := p.entries[key]
	valid := present && current.ID == grant.ID && time.Now().Before(current.deadline) && current.Exp > time.Now().Unix()
	p.mu.Unlock()
	if !valid {
		p.misses.Add(1)
		return false
	}
	p.hits.Add(1)
	now := time.Now()
	w.Header().Set("Date", now.UTC().Format(http.TimeFormat))
	if grant.Post {
		remaining := max(int64(0), int64(time.Until(grant.deadline)/time.Second))
		w.Header().Set("Cache-Control", "private, max-age="+strconv.FormatInt(min(int64(5), grant.Exp-now.Unix(), remaining), 10)+", must-revalidate")
	} else {
		etag := strconv.Quote("image-" + strings.TrimPrefix(r.URL.Path, "/api/images/"))
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Set("ETag", etag)
		if imageETagMatches(r.Header.Values("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	w.Header().Set("Content-Type", grant.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(grant.Size, 10))
	stats.local.Add(1)
	body := &regionalImageBody{io.NewSectionReader(f, grant.Offset, grant.Size), f, stats}
	copyStream(w, body)
	return true
}

func withRegionRequestStart(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), regionRequestStart{}, time.Now()))
}
