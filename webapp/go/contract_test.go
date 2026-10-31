package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type rejectTailReader struct{ reads int }

func (r *rejectTailReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads > 1 {
		panic("invalid JPEG tail must not be consumed")
	}
	copy(p, "not a jpeg")
	return len("not a jpeg"), nil
}

func TestProfileStreamValidation(t *testing.T) {
	bad := &rejectTailReader{}
	if p := prepareProfile(bad); p.ok || !p.invalidJPEG {
		t.Fatal("invalid JPEG accepted")
	}
	var original bytes.Buffer
	if err := jpeg.Encode(&original, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	original.WriteString("preserved trailer")
	p := prepareProfile(bytes.NewReader(original.Bytes()))
	if !p.ok || !bytes.Equal(p.data, original.Bytes()) {
		t.Fatal("original JPEG bytes were not preserved")
	}
}

func TestOversizedProfilePreservesDecodeFailureClassification(t *testing.T) {
	var original bytes.Buffer
	if err := jpeg.Encode(&original, image.NewRGBA(image.Rect(0, 0, 513, 32)), nil); err != nil {
		t.Fatal(err)
	}
	valid := original.Bytes()
	invalid := valid[:len(valid)-20]
	if _, err := jpeg.Decode(bytes.NewReader(invalid)); err == nil {
		t.Fatal("fixture must have invalid entropy data")
	}
	for _, tc := range []struct {
		name    string
		data    []byte
		invalid bool
	}{{"valid", valid, false}, {"truncated", invalid, true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := prepareProfile(bytes.NewReader(tc.data))
			if p.ok || p.invalidJPEG != tc.invalid {
				t.Fatalf("ok=%v invalidJPEG=%v, want rejection with invalidJPEG=%v", p.ok, p.invalidJPEG, tc.invalid)
			}
		})
	}
}

func TestHeaderValidationMode(t *testing.T) {
	t.Setenv("JPEG_VALIDATION", "header")
	if p := prepareProfile(bytes.NewBufferString("not a JPEG")); p.ok || !p.invalidJPEG {
		t.Fatal("non-JPEG must still be rejected")
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 128, 128)), nil); err != nil {
		t.Fatal(err)
	}
	truncated := b.Bytes()[:b.Len()-20]
	if _, err := jpeg.Decode(bytes.NewReader(truncated)); err == nil {
		t.Fatal("fixture must have invalid entropy data")
	}
	if p := prepareProfile(bytes.NewReader(truncated)); !p.ok || !bytes.Equal(p.data, truncated) {
		t.Fatal("header mode did not preserve the opaque JPEG payload")
	}
	b.Reset()
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 513, 2)), nil); err != nil {
		t.Fatal(err)
	}
	if p := prepareProfile(bytes.NewReader(b.Bytes())); p.ok || p.invalidJPEG {
		t.Fatal("dimension rejection must remain separate from invalid format")
	}
}

func BenchmarkSignedAlternatingExpiration(b *testing.B) {
	a := &App{appCore: &appCore{s: emptyState(), signatures: map[string]string{}}}
	a.s.Accounts.Set("owner", &Account{Secret: secretHash("key")})
	im := &Image{ID: "image", OwnerID: "owner"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.signed(im, 1800000000+int64(i%2))
	}
}

func TestResponseSnapshotSurvivesReset(t *testing.T) {
	a := fixtureApp(t)
	uid, _ := register(t, a, "snapshot")
	im := &Image{ID: id(), OwnerID: uid}
	a.put("image", im)
	u := *a.s.Accounts.Get(uid)
	u.ProfileImageID = &im.ID
	a.put("account", u)
	snapshot := a.me(&u)
	expected, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.reset(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 16; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			actual, err := json.Marshal(snapshot)
			if err != nil || !bytes.Equal(actual, expected) {
				t.Errorf("response changed after reset: %s %v", actual, err)
			}
		}()
	}
	wg.Wait()
}

func fixtureApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "images"), 0755)
	for _, n := range []string{"accounts", "administrators", "posting-windows", "friend-requests", "friendships", "follows", "blocks", "images/index.part-000", "posts", "realmojis.part-000", "comments"} {
		f, e := os.Create(filepath.Join(dir, n+".jsonl.gz"))
		if e != nil {
			t.Fatal(e)
		}
		g := gzip.NewWriter(f)
		g.Close()
		f.Close()
	}
	a, e := newApp(dir, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.journal.Close(); a.s.closeHistory() })
	a.put("admin", Administrator{ID: "admin", Username: "admin", Password: secretHash("password")})
	a.commit()
	return a
}
func call(t *testing.T, a http.Handler, method, path, token string, body any) (int, map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	var b io.Reader
	if body != nil {
		x, _ := json.Marshal(body)
		b = bytes.NewReader(x)
	}
	r := httptest.NewRequest(method, path, b)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	var v map[string]any
	json.Unmarshal(w.Body.Bytes(), &v)
	return w.Code, v, w
}
func requireCall(t *testing.T, a http.Handler, method, path, token string, body any, want int) map[string]any {
	t.Helper()
	s, v, w := call(t, a, method, path, token, body)
	if s != want {
		t.Fatalf("%s %s: %d want %d: %s", method, path, s, want, w.Body.String())
	}
	return v
}
func register(t *testing.T, a *App, name string) (string, string) {
	v := requireCall(t, a, "POST", "/api/auth/register", "", object{"username": name, "displayName": name, "region": "asia"}, 201)
	uid := v["user"].(map[string]any)["id"].(string)
	l := requireCall(t, a, "POST", "/api/auth/login", "", object{"username": name, "secretKey": v["secretKey"]}, 200)
	return uid, l["token"].(string)
}
func upload(t *testing.T, a *App, path, token string, fields map[string]string, want int) map[string]any {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	f, _ := mw.CreateFormFile("image", "image.jpg")
	jpeg.Encode(f, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	mw.Close()
	r := httptest.NewRequest("POST", path, &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("upload %s: %d %s", path, w.Code, w.Body.String())
	}
	var v map[string]any
	json.Unmarshal(w.Body.Bytes(), &v)
	return v
}
func TestPublicContract(t *testing.T) {
	a := fixtureApp(t)
	uid, ut := register(t, a, "alice")
	bare := httptest.NewRequest("GET", "/api/me", nil)
	bare.Header.Set("Authorization", ut)
	bw := httptest.NewRecorder()
	a.ServeHTTP(bw, bare)
	if bw.Code != 401 {
		t.Fatal("accepted a token without Bearer scheme")
	}
	var jpegBody bytes.Buffer
	jpeg.Encode(&jpegBody, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
	pr := httptest.NewRequest("PUT", "/api/me/profile-image", &jpegBody)
	pr.Header.Set("Content-Type", "image/jpeg")
	pr.Header.Set("Authorization", "Bearer "+ut)
	pw := httptest.NewRecorder()
	a.ServeHTTP(pw, pr)
	if pw.Code != 204 {
		t.Fatalf("profile image %d", pw.Code)
	}
	profileURL := requireCall(t, a, "GET", "/api/me", ut, nil, 200)["profileImageUrl"].(string)
	requireCall(t, a, "GET", profileURL, "", nil, 200)
	if err := os.Remove(filepath.Join(a.durable, "images", *a.s.Accounts.Get(uid).ProfileImageID)); err != nil {
		t.Fatal(err)
	}
	requireCall(t, a, "GET", profileURL, "", nil, 500)
	requireCall(t, a, "DELETE", "/api/me/profile-image", ut, nil, 204)
	requireCall(t, a, "GET", profileURL, "", nil, 403)
	vid, vt := register(t, a, "bob")
	_, wt := register(t, a, "carol")
	adm := requireCall(t, a, "POST", "/api/admin/auth/login", "", object{"username": "admin", "password": "password"}, 200)["token"].(string)
	requireCall(t, a, "PATCH", "/api/me", ut, object{"bio": "private"}, 200)
	v := requireCall(t, a, "GET", "/api/users/"+uid, vt, nil, 200)
	if v["bio"] != "" {
		t.Fatal("bio leaked")
	}
	q := requireCall(t, a, "POST", "/api/friend-requests", ut, object{"recipientId": vid}, 201)
	rid := q["relationId"].(string)
	requireCall(t, a, "POST", "/api/friend-requests/"+rid+"/accept", vt, nil, 200)
	if v = requireCall(t, a, "GET", "/api/users/"+uid, vt, nil, 200); v["bio"] != "private" {
		t.Fatal("friend bio missing")
	}
	requireCall(t, a, "PUT", "/api/admin/users/"+uid+"/certification", adm, nil, 204)
	requireCall(t, a, "PUT", "/api/follows/"+uid, vt, nil, 204)
	for _, path := range []string{"/api/users?q=alice", "/api/relationships/friends", "/api/relationships/following", "/api/relationships/followers", "/api/relationships/incoming-requests", "/api/relationships/outgoing-requests"} {
		requireCall(t, a, "GET", path, vt, nil, 200)
	}
	requireCall(t, a, "GET", "/api/posting-window/current", ut, nil, 200)
	requireCall(t, a, "POST", "/api/admin/posting-window", adm, object{"region": "asia", "localDate": "2026-11-01"}, 201)
	fields := map[string]string{"caption": "hello", "visibility": "friends", "location_kind": "none"}
	p := upload(t, a, "/api/posts", ut, fields, 201)
	pid := p["id"].(string)
	requireCall(t, a, "GET", "/api/posts/"+pid, vt, nil, 404)
	upload(t, a, "/api/posts", vt, fields, 201)
	requireCall(t, a, "GET", "/api/posts/"+pid, vt, nil, 200)
	requireCall(t, a, "GET", "/api/posts/"+pid, wt, nil, 404)
	for _, kind := range []string{"friends", "following", "public", "friends-of-friends"} {
		requireCall(t, a, "GET", "/api/feeds/"+kind, vt, nil, 200)
	}
	c := requireCall(t, a, "POST", "/api/posts/"+pid+"/comments", vt, object{"body": "hi"}, 201)
	requireCall(t, a, "GET", "/api/posts/"+pid+"/comments", vt, nil, 200)
	re := upload(t, a, "/api/posts/"+pid+"/realmojis", vt, map[string]string{"reactionType": "like"}, 201)
	requireCall(t, a, "GET", "/api/posts/"+pid+"/realmojis", ut, nil, 200)
	imageURL := re["imageUrl"].(string)
	requireCall(t, a, "GET", imageURL, "", nil, 200)
	upload(t, a, "/api/posts/"+pid+"/realmojis", vt, map[string]string{"reactionType": "smile"}, 201)
	requireCall(t, a, "GET", imageURL, "", nil, 403)
	requireCall(t, a, "DELETE", "/api/realmojis/"+re["id"].(string), vt, nil, 204)
	requireCall(t, a, "DELETE", "/api/comments/"+c["id"].(string), ut, nil, 204)
	requireCall(t, a, "PATCH", "/api/posts/"+pid, ut, object{"caption": "new", "visibility": "public"}, 200)
	requireCall(t, a, "GET", "/api/memories?month=2026-11", ut, nil, 200)
	requireCall(t, a, "GET", "/api/memories/2026-11-01", ut, nil, 200)
	requireCall(t, a, "GET", "/api/me/export", ut, nil, 200)
	requireCall(t, a, "PUT", "/api/blocks/"+uid, vt, nil, 204)
	requireCall(t, a, "GET", "/api/posts/"+pid, vt, nil, 404)
	requireCall(t, a, "DELETE", "/api/blocks/"+uid, vt, nil, 204)
	requireCall(t, a, "POST", "/api/admin/posting-window", adm, object{"region": "asia", "localDate": "2026-11-02"}, 201)
	requireCall(t, a, "PATCH", "/api/posts/"+pid, ut, object{"caption": "closed"}, 403)
	requireCall(t, a, "GET", "/api/posts/"+pid, ut, nil, 200)
	requireCall(t, a, "DELETE", "/api/posts/"+pid, ut, nil, 204)
	requireCall(t, a, "GET", p["imageUrl"].(string), "", nil, 403)
	p = upload(t, a, "/api/posts", ut, fields, 201)
	requireCall(t, a, "DELETE", "/api/posts/"+p["id"].(string), ut, nil, 204)
	p = upload(t, a, "/api/posts", ut, fields, 201)
	requireCall(t, a, "DELETE", "/api/posts/"+p["id"].(string), ut, nil, 204)
	upload(t, a, "/api/posts", ut, fields, 409)
	a.commit()
	restored, e := newApp(a.initial, a.durable)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.journal.Close()
	requireCall(t, restored, "GET", "/api/me", ut, nil, 200)
	requireCall(t, restored, "GET", "/api/posting-window/current", ut, nil, 200)
	requireCall(t, a, "POST", "/api/me/account-deletion", ut, nil, 204)
	requireCall(t, a, "GET", "/api/me", ut, nil, 401)
}
func TestSSEContract(t *testing.T) {
	a := fixtureApp(t)
	_, token := register(t, a, "listener")
	adm := requireCall(t, a, "POST", "/api/admin/auth/login", "", object{"username": "admin", "password": "password"}, 200)["token"].(string)
	server := httptest.NewServer(a)
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+"/api/notifications", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	requireCall(t, a, "POST", "/api/admin/posting-window", adm, object{"region": "asia", "localDate": "2026-11-01"}, 201)
	b, e := io.ReadAll(resp.Body)
	if e != nil || !strings.Contains(string(b), "postingWindowId") {
		t.Fatal(fmt.Sprintf("SSE %s: %v", b, e))
	}
}

func TestSearchNormalizationTracksProfileUpdates(t *testing.T) {
	a := fixtureApp(t)
	_, viewer := register(t, a, "searcher")
	uid, target := register(t, a, "target")
	requireCall(t, a, "PATCH", "/api/me", target, object{"displayName": "Straße"}, 200)
	v := requireCall(t, a, "GET", "/api/users?q=STRASSE", viewer, nil, 200)
	items := v["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != uid {
		t.Fatal("Unicode case folding did not match")
	}
	requireCall(t, a, "PATCH", "/api/me", target, object{"displayName": "Cafe\u0301"}, 200)
	v = requireCall(t, a, "GET", "/api/users?q=STRASSE", viewer, nil, 200)
	if len(v["items"].([]any)) != 0 {
		t.Fatal("old display name remained searchable")
	}
	v = requireCall(t, a, "GET", "/api/users?q=CAF%C3%89", viewer, nil, 200)
	if len(v["items"].([]any)) != 1 {
		t.Fatal("Unicode normalization did not match updated name")
	}
}

func TestPendingRequestIndexesFollowTransitionsAndReplay(t *testing.T) {
	a := fixtureApp(t)
	_, at := register(t, a, "request_a")
	bid, bt := register(t, a, "request_b")
	cid, ct := register(t, a, "request_c")
	request := requireCall(t, a, "POST", "/api/friend-requests", at, object{"recipientId": bid}, 201)
	count := func(app *App, token, kind string, want int) {
		t.Helper()
		v := requireCall(t, app, "GET", "/api/relationships/"+kind, token, nil, 200)
		if len(v["items"].([]any)) != want {
			t.Fatalf("%s: expected %d items", kind, want)
		}
	}
	count(a, at, "outgoing-requests", 1)
	count(a, bt, "incoming-requests", 1)
	restored, err := newApp(a.initial, a.durable)
	if err != nil {
		t.Fatal(err)
	}
	count(restored, bt, "incoming-requests", 1)
	restored.journal.Close()
	restored.s.closeHistory()
	requireCall(t, a, "POST", "/api/friend-requests/"+request["relationId"].(string)+"/cancel", at, nil, 200)
	count(a, at, "outgoing-requests", 0)
	count(a, bt, "incoming-requests", 0)
	requireCall(t, a, "POST", "/api/friend-requests", at, object{"recipientId": cid}, 201)
	requireCall(t, a, "PUT", "/api/blocks/"+cid, at, nil, 204)
	count(a, at, "outgoing-requests", 0)
	count(a, ct, "incoming-requests", 0)
	requireCall(t, a, "POST", "/api/friend-requests", bt, object{"recipientId": cid}, 201)
	requireCall(t, a, "POST", "/api/friend-requests", ct, object{"recipientId": bid}, 201)
	count(a, bt, "outgoing-requests", 0)
	count(a, ct, "incoming-requests", 0)
	count(a, bt, "friends", 1)
}
