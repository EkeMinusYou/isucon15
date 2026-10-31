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
	"sync/atomic"
	"testing"
	"time"
)

func TestRegionalMasterRoutesMovedUserAndInvalidatesImages(t *testing.T) {
	oldSecret, oldRegion := transportSecret, fixedRegion
	transportSecret, fixedRegion = "regional-master-test", "asia"
	t.Cleanup(func() { transportSecret, fixedRegion = oldSecret, oldRegion })
	fixture := fixtureApp(t)
	owner, err := newAppCore(fixture.initial, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.journal.Close(); owner.closeHistory() })
	var handlers [3]http.Handler
	var servers [3]*httptest.Server
	for i, region := range masterRegions {
		index := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handlers[index].ServeHTTP(w, r) }))
		defer servers[i].Close()
		t.Setenv("REGION_"+strings.ToUpper(region)+"_URL", servers[i].URL)
	}
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte(transportSecret), 0600); err != nil {
		t.Fatal(err)
	}
	var masters [3]*regionalMaster
	for i := 1; i < 3; i++ {
		t.Setenv("REGION", masterRegions[i])
		t.Setenv("OWNER_URL", servers[0].URL)
		t.Setenv("INITIAL_DIR", owner.initial)
		t.Setenv("TRANSPORT_KEY_FILE", key)
		t.Setenv("REGIONAL_STATE_DIR", t.TempDir())
		t.Setenv("MASTER_STATE_DIR", t.TempDir())
		t.Setenv("IMAGE_CACHE_DIR", owner.initial+"/images")
		handler, app, err := serviceHandler()
		if err != nil {
			t.Fatal(err)
		}
		handlers[i], masters[i] = handler, app.master
		defer app.master.close()
	}
	fixedRegion = "asia"
	local, err := newRegionalMaster(owner.initial, t.TempDir(), owner.initial+"/images", "asia", &regionalUploads{root: owner.durable})
	if err != nil {
		t.Fatal(err)
	}
	defer local.close()
	masters[0] = local
	if err := owner.enableRegionalMasters(local); err != nil {
		t.Fatal(err)
	}
	owner.put("admin", Administrator{ID: "admin", Username: "admin", Password: secretHash("password")})
	owner.commit()
	var centralImages atomic.Int64
	var centralCalls atomic.Int64
	handler, err := local.route(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		centralCalls.Add(1)
		if strings.HasPrefix(r.URL.Path, "/api/images/") {
			centralImages.Add(1)
		}
		owner.ServeHTTP(w, r)
	}))
	if err != nil {
		t.Fatal(err)
	}
	handlers[0] = handler
	request := func(region int, method, path, token, contentType string, body []byte, status int) []byte {
		t.Helper()
		r, err := http.NewRequest(method, servers[region].URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", contentType)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		b, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status {
			t.Fatalf("%s %s: status=%d body=%s err=%v", method, path, response.StatusCode, b, err)
		}
		return b
	}
	jsonRequest := func(region int, method, path, token string, value any, status int) object {
		t.Helper()
		b, _ := json.Marshal(value)
		result := request(region, method, path, token, "application/json", b, status)
		var data object
		if len(result) > 0 {
			if err := json.Unmarshal(result, &data); err != nil {
				t.Fatal(err)
			}
		}
		return data
	}
	registered := jsonRequest(1, "POST", "/api/auth/register", "", object{"username": "moved_master", "displayName": "Moved", "region": "europe"}, 201)
	uid := registered["user"].(map[string]any)["id"].(string)
	centralCalls.Store(0)
	login := jsonRequest(1, "POST", "/api/auth/login", "", object{"username": "moved_master", "secretKey": registered["secretKey"]}, 200)
	if centralCalls.Load() != 0 {
		t.Fatal("home login contacted the coordinator")
	}
	token := login["token"].(string)
	jsonRequest(1, "PATCH", "/api/me", token, object{"region": "america"}, 200)
	login = jsonRequest(2, "POST", "/api/auth/login", "", object{"username": "moved_master", "secretKey": registered["secretKey"]}, 200)
	token = login["token"].(string)
	if homeOfNewID(token) != "europe" {
		t.Fatal("location change moved session master")
	}
	var original bytes.Buffer
	jpeg.Encode(&original, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
	request(2, "PUT", "/api/me/profile-image", token, "image/jpeg", original.Bytes(), 204)
	profileID := *owner.s.Accounts.Get(uid).ProfileImageID
	if _, err := masters[1].files.open(profileID); err != nil {
		t.Fatal("fixed master did not retain original", err)
	}
	if _, err := masters[2].files.open(profileID); !os.IsNotExist(err) {
		t.Fatal("entry region retained moved user's original")
	}
	me := jsonRequest(2, "GET", "/api/me", token, nil, 200)
	imageURL := me["profileImageUrl"].(string)
	for i := range masterRegions {
		if b := request(i, "GET", imageURL, "", "", nil, 200); !bytes.Equal(b, original.Bytes()) {
			t.Fatal("original changed")
		}
	}
	if centralImages.Load() != 0 {
		t.Fatal("image request reached shared application")
	}
	request(2, "DELETE", "/api/me/profile-image", token, "", nil, 204)
	for i := range masterRegions {
		request(i, "GET", imageURL, "", "", nil, 403)
	}

	// The reactor's master checks the parent at the author's master each time.
	_, author := register(t, owner, "cross_master_author")
	admin := openWindow(t, owner, "2026-11-01")
	requireCall(t, owner, "POST", "/api/admin/posting-window", admin, object{"region": "america", "localDate": "2026-11-01"}, 201)
	postUpload := imageRequest("/api/posts", author, map[string]string{"visibility": "public", "location_kind": "none"})
	var post object
	centralCalls.Store(0)
	json.Unmarshal(request(0, "POST", "/api/posts", author, postUpload.contentType, postUpload.body, 201), &post)
	if centralCalls.Load() != 0 || owner.appCore.s.Posts.get(post["id"].(string)) != nil {
		t.Fatal("local post waited for or updated the coordinator")
	}
	viewerUpload := imageRequest("/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"})
	request(2, "POST", "/api/posts", token, viewerUpload.contentType, viewerUpload.body, 201)
	reactionPath := "/api/posts/" + post["id"].(string) + "/realmojis"
	reactionUpload := imageRequest(reactionPath, token, map[string]string{"reactionType": "like"})
	var reaction object
	json.Unmarshal(request(2, "POST", reactionPath, token, reactionUpload.contentType, reactionUpload.body, 201), &reaction)
	if owner.appCore.s.Posts.get(post["id"].(string)) == nil {
		t.Fatal("a coordinator read missed a completed regional post")
	}
	reactionURL := reaction["imageUrl"].(string)
	request(2, "GET", reactionURL, "", "", nil, 200)
	request(0, "DELETE", "/api/posts/"+post["id"].(string), author, "", nil, 204)
	request(2, "GET", reactionURL, "", "", nil, 403)

	// Signature failures reach the coordinator, which publishes a BAN to all homes.
	badURL := imageURL[:strings.Index(imageURL, "&sig=")+5] + strings.Repeat("A", 43)
	for i := 0; i < 3; i++ {
		request(2, "GET", badURL, "", "", nil, 403)
	}
	for _, master := range masters {
		master.mu.RLock()
		banned := time.Now().Before(master.s.Bans.Get("127.0.0.1"))
		master.mu.RUnlock()
		if !banned {
			t.Fatal("signature BAN was not published before the response")
		}
	}
}
