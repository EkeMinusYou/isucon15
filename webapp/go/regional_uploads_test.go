package main

import (
	"archive/zip"
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
)

func TestRegionalUploadsKeepOriginalsAndReplayStorageLocation(t *testing.T) {
	for _, region := range []string{"asia", "europe"} {
		t.Run(region, func(t *testing.T) { testRegionalUploads(t, region) })
	}
}

func testRegionalUploads(t *testing.T, region string) {
	oldSecret, oldRegion := transportSecret, fixedRegion
	transportSecret, fixedRegion = "regional-upload-test", "asia"
	t.Cleanup(func() { transportSecret, fixedRegion = oldSecret, oldRegion })
	owner := fixtureApp(t)
	uid, token := register(t, owner, "regional_upload_owner")
	requireCall(t, owner, "PATCH", "/api/me", token, object{"region": region}, 200)
	admin := openWindow(t, owner, "2026-11-01")
	if region != "asia" {
		requireCall(t, owner, "POST", "/api/admin/posting-window", admin, object{"region": region, "localDate": "2026-11-01"}, 201)
	}
	upstream := httptest.NewServer(owner)
	defer upstream.Close()
	keyFile := filepath.Join(t.TempDir(), "transport-key")
	if err := os.WriteFile(keyFile, []byte(transportSecret), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("REGION", region)
	t.Setenv("OWNER_URL", upstream.URL)
	t.Setenv("TRANSPORT_KEY_FILE", keyFile)
	t.Setenv("REGIONAL_STATE_DIR", root)
	t.Setenv("IMAGE_CACHE_DIR", t.TempDir())
	for _, peer := range []string{"ASIA", "EUROPE", "AMERICA"} {
		t.Setenv("REGION_"+peer+"_URL", "")
	}
	h, edge, err := serviceHandler()
	if err != nil {
		t.Fatal(err)
	}
	fixedRegion = "asia" // The real processes have separate region globals.
	regional := httptest.NewServer(h)
	defer regional.Close()
	t.Setenv("REGION_"+strings.ToUpper(region)+"_URL", regional.URL)
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
	var original bytes.Buffer
	if err := jpeg.Encode(&original, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	original.WriteString("original trailer")
	request("PUT", "/api/me/profile-image", token, "image/jpeg", original.Bytes(), 204)
	profileID := *owner.s.Accounts.Get(uid).ProfileImageID
	profile := owner.s.Images.get(profileID)
	if profile.StorageRegion != region {
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
	got := request("GET", imageURL, "", "", nil, 200)
	if !bytes.Equal(got, original.Bytes()) || edge.regionalImages.local.Load() != 1 {
		t.Fatal("regional original was not served locally")
	}
	if _, err := os.Stat(filepath.Join(owner.durable, "images", profileID)); !os.IsNotExist(err) {
		t.Fatal("local delivery fetched the original through the owner")
	}
	_, _, response := call(t, owner, "GET", imageURL, "", nil)
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), original.Bytes()) {
		t.Fatal("another region could not fetch the original from its origin")
	}
	q := imageRequest("/api/posts", token, map[string]string{"caption": "body", "visibility": "public", "location_kind": "none"})
	var post object
	if err := json.Unmarshal(request("POST", "/api/posts?caption=query", token, q.contentType, q.body, 201), &post); err != nil {
		t.Fatal(err)
	}
	stored := owner.s.Posts.get(post["id"].(string))
	if stored.Caption != "query" || owner.s.Images.get(stored.ImageID).StorageRegion != region {
		t.Fatal("multipart form precedence or image placement changed")
	}
	// A rejected registration must not retain its staged original.
	request("POST", "/api/posts", token, q.contentType, q.body, 409)
	pending, _ := os.ReadDir(filepath.Join(root, "pending", "images"))
	if len(pending) != 0 {
		t.Fatal("rejected upload retained a pending original")
	}
	_, peerToken := register(t, owner, "regional_upload_peer")
	requireCall(t, owner, "PATCH", "/api/me", peerToken, object{"region": region}, 200)
	peerUpload := imageRequest("/api/posts", peerToken, map[string]string{"visibility": "public", "location_kind": "none"})
	var peerPost object
	json.Unmarshal(request("POST", "/api/posts", peerToken, peerUpload.contentType, peerUpload.body, 201), &peerPost)
	reactionUpload := imageRequest("", token, map[string]string{"reactionType": "like"})
	var reaction object
	json.Unmarshal(request("POST", "/api/posts/"+peerPost["id"].(string)+"/realmojis", token, reactionUpload.contentType, reactionUpload.body, 201), &reaction)
	reactionImage := owner.s.Images.get(owner.s.Reactions.get(reaction["id"].(string)).ImageID)
	if reactionImage.StorageRegion != region {
		t.Fatal("RealMoji original was not placed in its upload region")
	}
	request("GET", reaction["imageUrl"].(string), "", "", nil, 200)
	archive := request("GET", "/api/me/export", token, "", nil, 200)
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(zr.File) != 2 {
		t.Fatal("regional originals could not be exported", err)
	}
	originalPost, err := os.ReadFile(filepath.Join(root, "images", stored.ImageID))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range zr.File {
		if file.Name == "images/000001.jpg" {
			reader, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || !bytes.Equal(data, originalPost) {
				t.Fatal("export changed the regional original")
			}
		}
	}
	request("POST", signatureComputePath, "", "application/json", []byte("[]"), 403)
	jobs, _ := json.Marshal([]signatureJob{{profileID, secretHash("test-key"), 1800000000}})
	computeReq, _ := http.NewRequest("POST", regional.URL+signatureComputePath, bytes.NewReader(jobs))
	computeReq.Header.Set("X-ISU-Transport", transportSecret)
	computeResp, err := http.DefaultClient.Do(computeReq)
	if err != nil {
		t.Fatal(err)
	}
	var signatures []string
	err = json.NewDecoder(computeResp.Body).Decode(&signatures)
	computeResp.Body.Close()
	if err != nil || computeResp.StatusCode != 200 || len(signatures) != 1 || signatures[0] != computeImageSignature(profileID, secretHash("test-key"), 1800000000) {
		t.Fatal("regional API and signature computation could not coexist")
	}
	restored, err := newApp(owner.initial, owner.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.s.closeHistory()
	if restored.s.Images.get(stored.ImageID).StorageRegion != region {
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
	request("GET", post["imageUrl"].(string), "", "", nil, 403)
	request("PUT", "/api/me/profile-image", "", "image/jpeg", original.Bytes(), 401)
	request("PUT", "/api/me/profile-image", token, "image/jpeg", []byte("not jpeg"), 400)
	request("GET", regionalFilePrefix+profileID, "", "", nil, 403)
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
