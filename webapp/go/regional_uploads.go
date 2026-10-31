package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	measurement "github.com/EkeMinusYou/isucon-template/tools/measurement/app"
)

const regionalUploadHeader = "X-ISU-Upload"
const regionalUploadAccepted = "X-ISU-Upload-Accepted"
const regionalImageUploaded = "X-ISU-Image-Uploaded"
const regionalFilePrefix = "/api/internal/region/images/"
const regionalResetPath = "/api/internal/region/reset-images"

type regionalUploadMetadata struct {
	ID            string
	Size          int64
	Valid         bool
	InvalidJPEG   bool
	StorageFailed bool
}

type regionalUploadKey struct{}

// Unregistered originals stay separate from accepted files. Initialize can
// remove accepted files without removing an upload still waiting to register.
type regionalUploads struct {
	root   string
	mu     sync.Mutex
	active map[string]bool
}

func newRegionalUploads(root string) (*regionalUploads, error) {
	u := &regionalUploads{root: root, active: map[string]bool{}}
	for _, dir := range []string{u.acceptedDir(), u.pendingDir()} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	// A process may have stopped between registration and promotion. Retain
	// those originals; only the owner's state can authorize their public use.
	files, err := os.ReadDir(u.pendingDir())
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if _, ok := uuidKey(file.Name()); ok && !file.IsDir() {
			if err := os.Rename(filepath.Join(u.pendingDir(), file.Name()), filepath.Join(u.acceptedDir(), file.Name())); err != nil {
				return nil, err
			}
		}
	}
	return u, nil
}

func (u *regionalUploads) acceptedDir() string { return filepath.Join(u.root, "images") }
func (u *regionalUploads) pendingDir() string  { return filepath.Join(u.root, "pending", "images") }

func isImageUpload(r *http.Request) (profile, multipart bool) {
	profile = r.URL.Path == "/api/me/profile-image" && r.Method == "PUT" && r.Header.Get("Content-Type") == "image/jpeg"
	multipart = r.Method == "POST" && (r.URL.Path == "/api/posts" || strings.HasSuffix(r.URL.Path, "/realmojis"))
	return
}

func (u *regionalUploads) prepare(a *App, w http.ResponseWriter, r *http.Request) (*http.Request, *preparedUpload) {
	fileID := id()
	u.mu.Lock()
	u.active[fileID] = true
	u.mu.Unlock()
	profile, _ := isImageUpload(r)
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	var p preparedUpload
	if profile {
		p = a.stageImageNamed(r.Body, 512, fileID)
	} else {
		max := 512
		if r.URL.Path == "/api/posts" {
			max = 4096
		}
		p = a.stageMultipartImageNamed(r, max, fileID)
	}
	p.stagedID, p.registrationUncertain = fileID, true
	metadata := regionalUploadMetadata{p.fileID, p.size, p.ok, p.invalidJPEG, p.storageErr != nil}
	b, _ := json.Marshal(metadata)
	r.Header.Set(regionalUploadHeader, base64.RawURLEncoding.EncodeToString(b))
	// Validation fields retain ParseMultipartForm's query/body precedence. The
	// original image is already durable locally, so it need not cross regions.
	body := ""
	if !profile {
		values := url.Values{}
		for _, key := range []string{"caption", "visibility", "location_kind", "latitude", "longitude", "locality", "reactionType"} {
			if entries := r.Form[key]; len(entries) > 0 {
				values[key] = entries[:1]
			}
		}
		body = values.Encode()
		r.URL.RawQuery = ""
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Body.Close()
	r.Body = io.NopCloser(strings.NewReader(body))
	r.GetBody = nil
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	r.Header.Del("Content-Length")
	r = r.WithContext(context.WithValue(r.Context(), regionalUploadKey{}, &p))
	return r, &p
}

func decodeRegionalUpload(r *http.Request) (preparedUpload, bool) {
	if !isInternal(r) || !validRegion(r.Header.Get("X-ISU-Region")) || r.Header.Get(regionalUploadHeader) == "" {
		return preparedUpload{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(r.Header.Get(regionalUploadHeader))
	var m regionalUploadMetadata
	if err != nil || json.Unmarshal(b, &m) != nil || m.Size < 0 || m.Size > 32<<20 {
		return preparedUpload{}, false
	}
	if _, ok := uuidKey(m.ID); !ok && !(m.ID == "" && m.StorageFailed) {
		return preparedUpload{}, false
	}
	p := preparedUpload{fileID: m.ID, size: m.Size, ok: m.Valid, invalidJPEG: m.InvalidJPEG, origin: r.Header.Get("X-ISU-Region")}
	if m.StorageFailed {
		p.storageErr = errors.New("regional image storage failed")
	}
	return p, true
}

func (u *regionalUploads) finish(resp *http.Response) {
	defer resp.Header.Del(regionalUploadAccepted)
	p, _ := resp.Request.Context().Value(regionalUploadKey{}).(*preparedUpload)
	if p == nil {
		return
	}
	p.registrationUncertain = false
	if p.fileID == "" || resp.Header.Get(regionalUploadAccepted) != p.fileID {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := os.Rename(filepath.Join(u.pendingDir(), p.fileID), filepath.Join(u.acceptedDir(), p.fileID)); err != nil {
		// The durable pending original remains usable through open().
		log.Printf("promote regional original: %v", err)
	}
	p.committed = true
}

func (u *regionalUploads) discard(p *preparedUpload) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.active, p.stagedID)
	// A lost upstream response does not prove rejection. Retain that original
	// until initialize so an accepted registration survives the transport error.
	if !p.committed && !p.registrationUncertain && p.fileID != "" {
		os.Remove(filepath.Join(u.pendingDir(), p.fileID))
	}
}

func (u *regionalUploads) open(id string) (*os.File, error) {
	if _, ok := uuidKey(id); !ok {
		return nil, os.ErrNotExist
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	f, err := os.Open(filepath.Join(u.acceptedDir(), id))
	if errors.Is(err, os.ErrNotExist) {
		return os.Open(filepath.Join(u.pendingDir(), id))
	}
	return f, err
}

func (u *regionalUploads) internal(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != regionalResetPath && !strings.HasPrefix(r.URL.Path, regionalFilePrefix) {
		return false
	}
	if !isInternal(r) {
		failure(w, 403, "forbidden")
		return true
	}
	if r.URL.Path == regionalResetPath && r.Method == "POST" {
		u.mu.Lock()
		err := os.RemoveAll(u.acceptedDir())
		if err == nil {
			err = os.MkdirAll(u.acceptedDir(), 0700)
		}
		if err == nil {
			var pending []os.DirEntry
			pending, err = os.ReadDir(u.pendingDir())
			for _, file := range pending {
				if !u.active[file.Name()] && !file.IsDir() {
					if removeErr := os.Remove(filepath.Join(u.pendingDir(), file.Name())); err == nil {
						err = removeErr
					}
				}
			}
		}
		u.mu.Unlock()
		if err != nil {
			failure(w, 500, "regional_reset_failed")
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		return true
	}
	if r.Method != "GET" || !strings.HasPrefix(r.URL.Path, regionalFilePrefix) {
		failure(w, 405, "method_not_allowed")
		return true
	}
	f, err := u.open(strings.TrimPrefix(r.URL.Path, regionalFilePrefix))
	if err != nil {
		failure(w, 404, "image_unavailable")
		return true
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		failure(w, 500, "image_unavailable")
		return true
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Cache-Control", "no-store")
	copyStream(w, f)
	return true
}

var regionalClient = &http.Client{Transport: measurement.NewUpstreamTransport(), Timeout: 10 * time.Second}

func regionalPeer(region string) string {
	return strings.TrimRight(os.Getenv("REGION_"+strings.ToUpper(region)+"_URL"), "/")
}

func regionalRequest(method, address string) (*http.Response, error) {
	r, err := http.NewRequest(method, address, nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("X-ISU-Transport", transportSecret)
	return regionalClient.Do(r)
}

func resetRegionalImages() error {
	for _, region := range []string{"asia", "europe", "america"} {
		if address := regionalPeer(region); address != "" {
			resp, err := regionalRequest("POST", address+regionalResetPath)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("reset images in %s: HTTP %d", region, resp.StatusCode)
			}
		}
	}
	return nil
}

// The authoritative process caches a remote original only when another region
// or an export needs it. Registration itself sends metadata, not image bytes.
func (a *App) fetchRegionalImage(im *Image) error {
	address := regionalPeer(im.StorageRegion)
	if address == "" {
		return fmt.Errorf("image storage region %q is unavailable", im.StorageRegion)
	}
	resp, err := regionalRequest("GET", address+regionalFilePrefix+im.ID)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("regional image: HTTP %d", resp.StatusCode)
	}
	f, err := os.CreateTemp(filepath.Join(a.durable, "images"), ".fetch-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	n, err := copyStream(f, io.LimitReader(resp.Body, im.Size+1))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if n != im.Size {
		return fmt.Errorf("regional original size: got %d, want %d", n, im.Size)
	}
	return os.Rename(f.Name(), filepath.Join(a.durable, "images", im.ID))
}
