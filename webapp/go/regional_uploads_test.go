package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegionalUploadsKeepOriginalsAndReplayStorageLocation(t *testing.T) {
	oldSecret, oldRegion := transportSecret, fixedRegion
	transportSecret, fixedRegion = "regional-upload-test", "asia"
	t.Cleanup(func() { transportSecret, fixedRegion = oldSecret, oldRegion })
	owner := fixtureApp(t)
	owner.imagePermits = newOwnerImagePermits()
	uid, token := register(t, owner, "regional_upload_owner")
	requireCall(t, owner, "PATCH", "/api/me", token, object{"region": "europe"}, 200)
	admin := openWindow(t, owner, "2026-11-01")
	requireCall(t, owner, "POST", "/api/admin/posting-window", admin, object{"region": "europe", "localDate": "2026-11-01"}, 201)
	upstream := httptest.NewServer(owner)
	defer upstream.Close()
	keyFile := filepath.Join(t.TempDir(), "transport-key")
	if err := os.WriteFile(keyFile, []byte(transportSecret), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("REGION", "europe")
	t.Setenv("OWNER_URL", upstream.URL)
	t.Setenv("TRANSPORT_KEY_FILE", keyFile)
	t.Setenv("REGIONAL_STATE_DIR", root)
	t.Setenv("IMAGE_CACHE_DIR", t.TempDir())
	t.Setenv("REGION_AMERICA_URL", "")
	h, edge, err := serviceHandler()
	if err != nil {
		t.Fatal(err)
	}
	fixedRegion = "asia" // The real processes have separate region globals.
	regional := httptest.NewServer(h)
	defer regional.Close()
	t.Setenv("REGION_EUROPE_URL", regional.URL)
	request := func(method, path, token, contentType string, body []byte, status int) []byte {
		t.Helper()
		r, err := http.NewRequest(method, regional.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", contentType)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != status {
			t.Fatalf("%s %s: status=%d, body=%s, error=%v", method, path, resp.StatusCode, b, err)
		}
		for key := range resp.Header {
			if strings.HasPrefix(strings.ToLower(key), "x-isu-") {
				t.Fatalf("internal header exposed: %s", key)
			}
		}
		return b
	}
	eventuallyRevoked := func(path string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			response, err := http.Get(regional.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == 403 {
				return
			}
			if response.StatusCode != 200 || time.Now().After(deadline) {
				t.Fatalf("image was not revoked within five seconds: status %d", response.StatusCode)
			}
			time.Sleep(time.Millisecond)
		}
	}
	var original bytes.Buffer
	if err := jpeg.Encode(&original, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	original.WriteString("original trailer")
	request("PUT", "/api/me/profile-image", token, "image/jpeg", original.Bytes(), 204)
	profileID := *owner.s.Accounts.Get(uid).ProfileImageID
	profile := owner.s.Images.get(profileID)
	if profile.StorageRegion != "europe" {
		t.Fatal("storage region missing from accepted image")
	}
	if _, err := os.Stat(filepath.Join(owner.durable, "images", profileID)); !os.IsNotExist(err) {
		t.Fatal("registration transferred the original to the owner")
	}
	var me object
	if err := json.Unmarshal(request("GET", "/api/me", token, "", nil, 200), &me); err != nil {
		t.Fatal(err)
	}
	imageURL := me["profileImageUrl"].(string)
	ownerImageRequests := owner.routeMetrics[metricRoute("/api/images/")].requests.Load()
	got := request("GET", imageURL, "", "", nil, 200)
	if !bytes.Equal(got, original.Bytes()) || edge.regionalImages.local.Load() != 1 {
		t.Fatal("regional original was not served locally")
	}
	if edge.regionalPermits.hits.Load() != 1 {
		t.Fatal("API response did not preauthorize local image delivery")
	}
	if owner.routeMetrics[metricRoute("/api/images/")].requests.Load() != ownerImageRequests {
		t.Fatal("valid regional permission still consulted the owner")
	}
	request("GET", imageURL+"x", "", "", nil, 403)
	if _, err := os.Stat(filepath.Join(owner.durable, "images", profileID)); !os.IsNotExist(err) {
		t.Fatal("local delivery fetched the original through the owner")
	}
	_, _, response := call(t, owner, "GET", imageURL, "", nil)
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), original.Bytes()) {
		t.Fatal("another region could not fetch the original from its origin")
	}
	request("PUT", "/api/me/profile-image", token, "image/jpeg", original.Bytes(), 204)
	eventuallyRevoked(imageURL)
	q := imageRequest("/api/posts", token, map[string]string{"caption": "body", "visibility": "public", "location_kind": "none"})
	var post object
	if err := json.Unmarshal(request("POST", "/api/posts?caption=query", token, q.contentType, q.body, 201), &post); err != nil {
		t.Fatal(err)
	}
	stored := owner.s.Posts.get(post["id"].(string))
	if stored.Caption != "query" || owner.s.Images.get(stored.ImageID).StorageRegion != "europe" {
		t.Fatal("multipart form precedence or image placement changed")
	}
	// A rejected registration must not retain its staged original.
	request("POST", "/api/posts", token, q.contentType, q.body, 409)
	pending, _ := os.ReadDir(filepath.Join(root, "pending", "images"))
	if len(pending) != 0 {
		t.Fatal("rejected upload retained a pending original")
	}
	_, peerToken := register(t, owner, "regional_upload_peer")
	requireCall(t, owner, "PATCH", "/api/me", peerToken, object{"region": "europe"}, 200)
	peerUpload := imageRequest("/api/posts", peerToken, map[string]string{"visibility": "public", "location_kind": "none"})
	var peerPost object
	json.Unmarshal(request("POST", "/api/posts", peerToken, peerUpload.contentType, peerUpload.body, 201), &peerPost)
	reactionUpload := imageRequest("", token, map[string]string{"reactionType": "like"})
	var reaction object
	json.Unmarshal(request("POST", "/api/posts/"+peerPost["id"].(string)+"/realmojis", token, reactionUpload.contentType, reactionUpload.body, 201), &reaction)
	reactionImage := owner.s.Images.get(owner.s.Reactions.get(reaction["id"].(string)).ImageID)
	if reactionImage.StorageRegion != "europe" {
		t.Fatal("RealMoji original was not placed in its upload region")
	}
	request("GET", reaction["imageUrl"].(string), "", "", nil, 200)
	request("POST", "/api/posts/"+peerPost["id"].(string)+"/realmojis", token, reactionUpload.contentType, reactionUpload.body, 201)
	eventuallyRevoked(reaction["imageUrl"].(string))
	restored, err := newApp(owner.initial, owner.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.s.closeHistory()
	if restored.s.Images.get(stored.ImageID).StorageRegion != "europe" {
		t.Fatal("storage location was lost during replay")
	}
	files, err := newRegionalUploads(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := files.open(stored.ImageID)
	if err != nil {
		t.Fatal("original was lost during regional restart", err)
	}
	f.Close()
	request("DELETE", "/api/posts/"+stored.ID, token, "", nil, 204)
	eventuallyRevoked(post["imageUrl"].(string))
	request("PUT", "/api/me/profile-image", "", "image/jpeg", original.Bytes(), 401)
	request("PUT", "/api/me/profile-image", token, "image/jpeg", []byte("not jpeg"), 400)
	request("GET", regionalFilePrefix+profileID, "", "", nil, 403)
	request("GET", "/api/posts/"+peerPost["id"].(string), peerToken, "", nil, 200)
	validURL := peerPost["imageUrl"].(string)
	request("GET", validURL, "", "", nil, 200)
	parts := strings.Split(validURL, "sig=")
	first := "A"
	if parts[1][0] == 'A' {
		first = "B"
	}
	invalidURL := parts[0] + "sig=" + first + parts[1][1:]
	for range 3 {
		request("GET", invalidURL, "", "", nil, 403)
	}
	eventuallyRevoked(validURL)
	// Reset also retires an unconfirmed upload whose response was lost.
	waitingID := id()
	if err := os.WriteFile(filepath.Join(files.pendingDir(), waitingID), original.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	requireCall(t, owner, "POST", "/api/initialize", "", nil, 200)
	accepted, _ := os.ReadDir(files.acceptedDir())
	if len(accepted) != 0 {
		t.Fatal("initialize retained old accepted originals")
	}
	if _, err := os.Stat(filepath.Join(files.pendingDir(), waitingID)); !os.IsNotExist(err) {
		t.Fatal("initialize retained an abandoned upload")
	}
	// A request still staging/registering its original is protected from reset.
	files.active[waitingID] = true
	os.WriteFile(filepath.Join(files.pendingDir(), waitingID), original.Bytes(), 0600)
	reset := httptest.NewRequest("POST", regionalResetPath, nil)
	reset.Header.Set("X-ISU-Transport", transportSecret)
	w := httptest.NewRecorder()
	files.internal(w, reset)
	if _, err := os.Stat(filepath.Join(files.pendingDir(), waitingID)); err != nil || w.Code != 204 {
		t.Fatal("initialize removed an upload still being registered")
	}
}
