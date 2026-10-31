package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const masterPrefix = "/api/internal/master/"

func masterRequest(ctx context.Context, region, path string, value any) (*http.Response, error) {
	address := regionalPeer(region)
	if address == "" {
		return nil, fmt.Errorf("master %s is not configured", region)
	}
	var body io.Reader
	if value != nil {
		b, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	method := "GET"
	if value != nil {
		method = "POST"
	}
	r, err := http.NewRequestWithContext(ctx, method, address+masterPrefix+path, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("X-ISU-Transport", transportSecret)
	r.Header.Set("X-ISU-Region", "asia")
	if path == "reset" {
		client := *regionalClient
		client.Timeout = 120 * time.Second
		return client.Do(r)
	}
	return regionalClient.Do(r)
}

func (m *regionalMaster) internal(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, masterPrefix) {
		return false
	}
	if !isInternal(r) {
		failure(w, 403, "forbidden")
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	switch strings.TrimPrefix(r.URL.Path, masterPrefix) {
	case "commit":
		var txns []masterTransaction
		if r.Method != "POST" || !decode(r, &txns) || len(txns) > 256 {
			failure(w, 400, "invalid_transaction")
			break
		}
		if err := m.commit(txns); err != nil {
			failure(w, 409, "master_conflict")
			break
		}
		w.WriteHeader(204)
	case "reset":
		var v struct{ Epoch string }
		if r.Method != "POST" || !decode(r, &v) || v.Epoch == "" {
			failure(w, 400, "invalid_epoch")
			break
		}
		if err := m.reset(v.Epoch); err != nil {
			failure(w, 500, "master_reset_failed")
			break
		}
		w.WriteHeader(204)
	case "epoch":
		m.mu.RLock()
		epoch := m.epoch
		m.mu.RUnlock()
		output(w, 200, object{"epoch": epoch})
	case "auth":
		var v struct{ UserID, SecretKey string }
		if r.Method != "POST" || !decode(r, &v) {
			failure(w, 400, "invalid_credentials")
			break
		}
		m.mu.RLock()
		u := m.s.Accounts.Get(v.UserID)
		valid := u != nil && u.Status == "active" && hmac.Equal([]byte(u.Secret), []byte(secretHash(v.SecretKey)))
		m.mu.RUnlock()
		output(w, 200, object{"valid": valid})
	default:
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, masterPrefix+"post/") {
			m.mu.RLock()
			valid := m.validPost(strings.TrimPrefix(r.URL.Path, masterPrefix+"post/"))
			m.mu.RUnlock()
			output(w, 200, object{"valid": valid})
		} else {
			return false
		}
	}
	return true
}

func (m *regionalMaster) validPost(id string) bool {
	p := m.s.Posts.get(id)
	return p != nil && p.DeletedAt == nil && m.s.active(p.AuthorID)
}

// The caller holds the local master read lock. A foreign parent is returned
// explicitly and checked at its own master, never through a TTL permission.
func (m *regionalMaster) imageValid(im *Image) (bool, string) {
	if im == nil || !m.s.active(im.OwnerID) {
		return false, ""
	}
	u := m.s.Accounts.Get(im.OwnerID)
	if u.ProfileImageID != nil && *u.ProfileImageID == im.ID {
		return true, ""
	}
	switch im.Kind {
	case 'p':
		return m.validPost(im.Ref), ""
	case 'r':
		v := m.s.Reactions.get(im.Ref)
		if v == nil || v.DeletedAt != nil || v.ReplacedAt != nil {
			return false, ""
		}
		home := indexedHome(m.homes.posts, v.PostID)
		if home == m.region {
			return m.validPost(v.PostID), ""
		}
		return true, v.PostID
	}
	return false, ""
}

func (m *regionalMaster) parentValid(ctx context.Context, id string) (bool, error) {
	home := indexedHome(m.homes.posts, id)
	resp, err := masterRequest(ctx, home, "post/"+id, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var result struct{ Valid bool }
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("parent master: HTTP %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	return result.Valid, err
}

func (m *regionalMaster) reportSignatureFailure(r *http.Request) error {
	resp, err := masterRequest(r.Context(), "asia", "failure", object{"ip": ipOf(r), "region": requestRegion(r), "kind": "signature"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 204 {
		return fmt.Errorf("BAN coordinator: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (m *regionalMaster) getImage(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "no-store")
	m.mu.RLock()
	bannedAtEntry := time.Now().Before(m.s.Bans.Get(ipOf(r)))
	m.mu.RUnlock()
	if bannedAtEntry {
		failure(w, 403, "ip_banned")
		return
	}
	q := r.URL.Query()
	es, ss := q["exp"], q["sig"]
	if len(es) != 1 || len(ss) != 1 {
		failure(w, 403, "image_forbidden")
		return
	}
	exp, err := strconv.ParseInt(es[0], 10, 64)
	raw, decodeErr := base64.RawURLEncoding.DecodeString(ss[0])
	if err != nil || strconv.FormatInt(exp, 10) != es[0] || exp <= time.Now().Unix() || decodeErr != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != ss[0] {
		failure(w, 403, "image_forbidden")
		return
	}
	m.mu.RLock()
	if time.Now().Before(m.s.Bans.Get(ipOf(r))) {
		m.mu.RUnlock()
		failure(w, 403, "ip_banned")
		return
	}
	im := m.s.Images.get(id)
	if im == nil || !m.s.active(im.OwnerID) {
		m.mu.RUnlock()
		failure(w, 403, "image_forbidden")
		return
	}
	copyImage := *im
	secret := m.s.Accounts.Get(im.OwnerID).Secret
	locallyValid, _ := m.imageValid(im)
	m.mu.RUnlock()
	if !hmac.Equal([]byte(ss[0]), []byte(m.signer.signImage(id, secret, exp))) {
		if err := m.reportSignatureFailure(r); err != nil {
			failure(w, 503, "image_unavailable")
			return
		}
		failure(w, 403, "image_forbidden")
		return
	}
	if !locallyValid {
		failure(w, 403, "image_forbidden")
		return
	}
	var file *os.File
	if copyImage.Part != "" {
		file, err = os.Open(filepath.Join(m.imageRoot, copyImage.Part))
	} else {
		file, err = m.files.open(copyImage.ID)
	}
	if err != nil {
		failure(w, 500, "image_unavailable")
		return
	}
	defer file.Close()
	m.mu.RLock()
	valid, parent := m.imageValid(m.s.Images.get(id))
	m.mu.RUnlock()
	if valid && parent != "" {
		valid, err = m.parentValid(r.Context(), parent)
		if err != nil {
			failure(w, 503, "image_unavailable")
			return
		}
	}
	m.mu.RLock()
	stillValid, _ := m.imageValid(m.s.Images.get(id))
	banned := time.Now().Before(m.s.Bans.Get(ipOf(r)))
	checkedAt := time.Now()
	m.mu.RUnlock()
	if banned {
		failure(w, 403, "ip_banned")
		return
	}
	if !valid || !stillValid || exp <= checkedAt.Unix() {
		failure(w, 403, "image_forbidden")
		return
	}
	w.Header().Set("Date", checkedAt.UTC().Format(http.TimeFormat))
	if copyImage.Kind == 'p' {
		w.Header().Set("Cache-Control", "private, max-age="+strconv.FormatInt(min(int64(5), exp-checkedAt.Unix()), 10)+", must-revalidate")
	} else {
		etag := strconv.Quote("image-" + id)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, no-cache")
		if imageETagMatches(r.Header.Values("If-None-Match"), etag) {
			w.WriteHeader(304)
			return
		}
	}
	w.Header().Set("Content-Type", copyImage.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(copyImage.Size, 10))
	n, _ := copyStream(w, io.NewSectionReader(file, copyImage.Offset, copyImage.Size))
	if m.stats != nil {
		m.stats.local.Add(1)
		m.stats.bytes.Add(uint64(n))
	}
}
