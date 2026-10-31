package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/jpeg"
	"io"
	"isuconapp/internal/jpegvalidate"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type object = map[string]any

type deferredResponse struct {
	*httptest.ResponseRecorder
	ctx   context.Context
	value any
	json  bool
	work  func(http.ResponseWriter)
}

func (d *deferredResponse) finish() {
	if d.json && d.value != nil {
		prefetchImageSignatures(d.ctx, d.value)
		json.NewEncoder(d.Body).Encode(d.value)
	}
}
func responseWork(w http.ResponseWriter, work func(http.ResponseWriter)) {
	if d, ok := w.(*deferredResponse); ok {
		d.work = work
	} else {
		work(w)
	}
}

type uploadKey struct{}
type preparedUpload struct {
	data        []byte
	ok          bool
	invalidJPEG bool
	fileID      string
	size        int64
	storageErr  error
	committed   bool
}

func validateJPEGEntropy() bool { return os.Getenv("JPEG_VALIDATION") != "header" }

// Validate before taking the state lock. The tee retains original bytes only
// after the JPEG header has been accepted; malformed bodies need not be read.
func prepareProfile(body io.Reader) preparedUpload {
	var b bytes.Buffer
	r := io.TeeReader(body, &b)
	cfg, err := jpeg.DecodeConfig(r)
	if err != nil {
		return preparedUpload{invalidJPEG: true}
	}
	if validateJPEGEntropy() {
		header := append([]byte(nil), b.Bytes()...)
		if err = jpegvalidate.Validate(io.MultiReader(bytes.NewReader(header), r)); err != nil {
			return preparedUpload{invalidJPEG: true}
		}
	}
	// Decode failures count toward the abuse limit even when dimensions also
	// exceed the profile limit. A valid oversized JPEG does not.
	if cfg.Width > 512 || cfg.Height > 512 {
		return preparedUpload{}
	}
	if _, err = io.Copy(&b, body); err != nil {
		return preparedUpload{invalidJPEG: true}
	}
	return preparedUpload{data: b.Bytes(), ok: true}
}

func output(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if d, ok := w.(*deferredResponse); ok {
		d.value, d.json = v, true
		return
	}
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}
func failure(w http.ResponseWriter, status int, code string) {
	output(w, status, object{"error": code})
}
func decode(r *http.Request, v any) bool { return json.NewDecoder(r.Body).Decode(v) == nil }
func validText(s string, min, max int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) >= min && utf8.RuneCountInString(s) <= max
}
func foldSearch(s string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(s)))
}

var usernameRE = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

func validRegion(s string) bool { return s == "asia" || s == "europe" || s == "america" }
func validVisibility(s string) bool {
	return s == "public" || s == "friends" || s == "friends_of_friends"
}
func secretHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func ipOf(r *http.Request) string {
	if x := r.Header.Get("X-Client-IP"); x != "" {
		return x
	}
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return h
}
func (a *App) failIP(r *http.Request, kind string, limit int, period time.Duration) {
	key := r.Header.Get("X-ISU-Source") + "/" + kind + "/" + ipOf(r)
	now := time.Now()
	xs := a.failures.Get(key)[:0]
	for _, t := range a.failures.Get(key) {
		if now.Sub(t) < period {
			xs = append(xs, t)
		}
	}
	xs = append(xs, now)
	if len(xs) >= limit {
		until := now.Add(2 * time.Minute)
		if a.s.Bans.Get(ipOf(r)).Before(until) {
			a.put("ban", banChange{ipOf(r), until})
		}
		xs = nil
	}
	a.failures.Set(key, xs)
}
func (a *App) auth(w http.ResponseWriter, r *http.Request, region bool) *Account {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		failure(w, 401, "invalid_session")
		return nil
	}
	t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	u := a.s.Accounts.Get(a.s.Sessions.Get(t))
	if u == nil || u.Status != "active" {
		failure(w, 401, "invalid_session")
		return nil
	}
	setMeasurementIdentity(r, u.ID)
	if isInternal(r) {
		w.Header().Set("X-ISU-Identity", u.ID)
	}
	if region && u.Region != requestRegion(r) {
		failure(w, 403, "region_mismatch")
		return nil
	}
	return u
}
func (a *App) admin(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		failure(w, 401, "invalid_administrator_session")
		return false
	}
	t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if a.s.AdminSessions.Get(t) == "" {
		failure(w, 401, "invalid_administrator_session")
		return false
	}
	return true
}
func requestRegion(r *http.Request) string {
	if x := r.Header.Get("X-ISU-Region"); x != "" {
		return x
	}
	return fixedRegion
}
func (a *App) signed(im *Image, exp int64) string {
	return a.signImage(im.ID, a.s.Accounts.Get(im.OwnerID).Secret, exp)
}
func (a *App) signImage(imageID, secret string, exp int64) string {
	key := signatureKey(imageID, secret, exp)
	value, flight, owner := a.reserveSignature(key)
	if value != "" {
		return value
	}
	if !owner {
		<-flight.done
		return flight.value
	}
	a.signatureMisses.Add(1)
	started := time.Now()
	value = computeImageSignature(imageID, secret, exp)
	a.signatureNS.Add(uint64(time.Since(started)))
	a.finishSignature(key, flight, value)
	return value
}

func computeImageSignature(imageID, secret string, exp int64) string {
	k, _ := hex.DecodeString(secret)
	mac := hmac.New(sha256.New, k)
	v := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	var sum [32]byte
	for i := 0; i < 1000; i++ {
		mac.Reset()
		mac.Write(v)
		v = mac.Sum(sum[:0])
	}
	return base64.RawURLEncoding.EncodeToString(v)
}

type imageReference struct {
	app        *App
	id, secret string
	exp        int64
}

func (v imageReference) MarshalJSON() ([]byte, error) {
	s := "/api/images/" + v.id + "?exp=" + strconv.FormatInt(v.exp, 10) + "&sig=" + v.app.signImage(v.id, v.secret, v.exp)
	return json.Marshal(s)
}

// Hourly expiry boundaries reuse signatures across responses. Every issued
// URL remains valid for more than 23 hours and at most 24 hours.
func imageExpiry(now time.Time) int64 {
	return now.Unix()/3600*3600 + 86400
}

func (a *App) imageURL(i string) any {
	im := a.s.Images.get(i)
	if im == nil {
		return nil
	}
	exp := imageExpiry(time.Now())
	return imageReference{a, i, a.s.Accounts.Get(im.OwnerID).Secret, exp}
}
func (a *App) user(u *Account, bio bool) object {
	b := ""
	if bio {
		b = u.Bio
	}
	var pic any
	if u.ProfileImageID != nil {
		pic = a.imageURL(*u.ProfileImageID)
	}
	return object{"id": u.ID, "username": u.Username, "displayName": u.DisplayName, "bio": b, "profileImageUrl": pic, "accountType": u.AccountType}
}
func sessionUser(u *Account) object {
	return object{"id": u.ID, "username": u.Username, "displayName": u.DisplayName, "bio": u.Bio, "region": u.Region, "accountType": u.AccountType, "status": u.Status}
}
func (a *App) me(u *Account) object {
	m := sessionUser(u)
	m["profileImageUrl"] = nil
	if u.ProfileImageID != nil {
		m["profileImageUrl"] = a.imageURL(*u.ProfileImageID)
	}
	return m
}
func windowJSON(v *Window) object {
	return object{"id": v.ID, "region": v.Region, "localDate": v.LocalDate, "startsAt": v.StartsAt, "onTimeDeadline": v.Deadline}
}
func (a *App) post(p *Post) object {
	w := a.s.Windows.Get(p.WindowID)
	late := int64(p.PostedAt.Sub(w.Deadline) / time.Second)
	if late < 0 {
		late = 0
	}
	return object{"id": p.ID, "author": a.user(a.s.Accounts.Get(p.AuthorID), true), "postingWindowId": p.WindowID, "imageUrl": a.imageURL(p.ImageID), "caption": p.Caption, "visibility": p.Visibility, "location": p.Location, "postedAt": p.PostedAt, "onTime": !p.PostedAt.After(w.Deadline), "lateSeconds": late}
}
func (a *App) comment(c *Comment) object {
	return object{"id": c.ID, "author": a.user(a.s.Accounts.Get(c.AuthorID), true), "body": c.Body, "createdAt": c.CreatedAt}
}
func (a *App) reaction(c *Reaction) object {
	return object{"id": c.ID, "reactor": a.user(a.s.Accounts.Get(c.ReactorID), true), "reactionType": c.Type, "imageUrl": a.imageURL(c.ImageID), "createdAt": c.CreatedAt}
}
func page(w http.ResponseWriter, x []object) {
	more := len(x) > 50
	if more {
		x = x[:50]
	}
	if x == nil {
		x = []object{}
	}
	output(w, 200, object{"items": x, "hasMore": more})
}
func pageCursor(w http.ResponseWriter, r *http.Request, timeName, idName string) (time.Time, string, bool) {
	q := r.URL.Query()
	t, i := q.Get(timeName), q.Get(idName)
	if (t == "") != (i == "") {
		failure(w, 400, "invalid_pagination")
		return time.Time{}, "", false
	}
	if t == "" {
		return time.Time{}, "", true
	}
	v, e := time.Parse(time.RFC3339Nano, t)
	if e != nil {
		failure(w, 400, "invalid_pagination")
		return v, "", false
	}
	return v, i, true
}
func before(t time.Time, i string, ct time.Time, ci string) bool {
	return ci == "" || t.Before(ct) || (t.Equal(ct) && i < ci)
}
func after(t time.Time, i string, ct time.Time, ci string) bool {
	return ci == "" || t.After(ct) || (t.Equal(ct) && i > ci)
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Write([]byte("ok\n"))
		return
	}
	a = &App{appCore: a.appCore}
	if r.URL.Path == "/api/notifications" && r.Method == "GET" {
		a.notifications(w, r)
		return
	}
	profile := r.URL.Path == "/api/me/profile-image" && r.Method == "PUT" && r.Header.Get("Content-Type") == "image/jpeg"
	multipart := r.Method == "POST" && (r.URL.Path == "/api/posts" || strings.HasSuffix(r.URL.Path, "/realmojis"))
	if profile || multipart {
		// Reject banned or unauthenticated uploads before retaining their bodies.
		a.mu.RLock()
		a.globalGate.RLock()
		check := httptest.NewRecorder()
		if time.Now().Before(a.s.Bans.Get(ipOf(r))) {
			failure(check, 403, "ip_banned")
		} else {
			a.auth(check, r, !profile)
		}
		a.globalGate.RUnlock()
		a.mu.RUnlock()
		if check.Code != 200 {
			for k, vs := range check.Header() {
				w.Header()[k] = vs
			}
			w.WriteHeader(check.Code)
			w.Write(check.Body.Bytes())
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
		var p preparedUpload
		if !validateJPEGEntropy() {
			if profile {
				p = a.stageImage(r.Body, 512)
			} else {
				max := 512
				if r.URL.Path == "/api/posts" {
					max = 4096
				}
				p = a.stageMultipartImage(r, max)
			}
		} else if profile {
			p = prepareProfile(r.Body)
		} else {
			max := 512
			if r.URL.Path == "/api/posts" {
				max = 4096
			}
			p.data, p.ok = multipartImage(r, max)
			if r.MultipartForm != nil {
				defer r.MultipartForm.RemoveAll()
			}
		}
		if p.ok && validateJPEGEntropy() {
			started := time.Now()
			p.fileID, p.size = id(), int64(len(p.data))
			p.storageErr = os.WriteFile(filepath.Join(a.durable, "images", p.fileID), p.data, 0600)
			a.uploadStageNS.Add(uint64(time.Since(started)))
			a.uploadStageBytes.Add(uint64(p.size))
		}
		if p.fileID != "" {
			defer func() {
				if !p.committed {
					os.Remove(filepath.Join(a.durable, "images", p.fileID))
				}
			}()
		}
		// Persistence is complete before waiting for state. Keep only metadata
		// while the request waits for its acceptance decision and response.
		p.data = nil
		r = r.WithContext(context.WithValue(r.Context(), uploadKey{}, &p))
	} else if r.Body != nil {
		b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if e != nil {
			failure(w, 413, "body_too_large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	rr := &deferredResponse{ResponseRecorder: httptest.NewRecorder(), ctx: r.Context()}
	stats := &a.routeMetrics[metricRoute(r.URL.Path)]
	started := time.Now()
	readOnly := r.Method == "GET"
	unlock := a.lockRequest(r)
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	locked := time.Now()
	stats.waitNS.Add(uint64(locked.Sub(started)))
	stats.requests.Add(1)
	if time.Now().Before(a.s.Bans.Get(ipOf(r))) {
		failure(rr, 403, "ip_banned")
	} else {
		a.dispatch(rr, r)
	}
	if !readOnly {
		a.commit()
	}
	stats.holdNS.Add(uint64(time.Since(locked)))
	unlock()
	unlock = nil
	started = time.Now()
	for k, vs := range rr.Header() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if rr.work != nil {
		// Stream the captured response after releasing state. Image and ZIP
		// bytes must not be materialized again in the response recorder.
		rr.work(w)
		stats.responseNS.Add(uint64(time.Since(started)))
		return
	}
	rr.finish()
	stats.responseNS.Add(uint64(time.Since(started)))
	w.WriteHeader(rr.Code)
	io.Copy(w, rr.Body)
}
func (a *App) dispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method
	s := a.s
	if path == "/api/initialize" && method == "POST" {
		if e := a.reset(); e != nil {
			failure(w, 500, "initialization_failed")
			return
		}
		output(w, 200, object{"language": "go", "agent": "Codex", "harness": "Codex", "model": "GPT-6"})
		return
	}
	if path == "/api/auth/register" && method == "POST" {
		var v struct{ Username, DisplayName, Region string }
		if !decode(r, &v) || !usernameRE.MatchString(v.Username) || !validText(v.DisplayName, 1, 100) || !validRegion(v.Region) {
			failure(w, 400, "invalid_registration")
			return
		}
		if s.Names.Get(v.Username) != "" {
			failure(w, 409, "username_taken")
			return
		}
		secret := id()
		u := &Account{ID: id(), Username: v.Username, Secret: secretHash(secret), DisplayName: v.DisplayName, Region: v.Region, AccountType: "regular", Status: "active"}
		a.put("account", u)
		output(w, 201, object{"secretKey": secret, "user": sessionUser(u)})
		return
	}
	if path == "/api/auth/login" && method == "POST" {
		var v struct{ Username, SecretKey string }
		decode(r, &v)
		u := s.Accounts.Get(s.Names.Get(v.Username))
		if !usernameRE.MatchString(v.Username) || u == nil || u.Status != "active" || !hmac.Equal([]byte(u.Secret), []byte(secretHash(v.SecretKey))) {
			a.failIP(r, "login", 10, 10*time.Second)
			failure(w, 401, "invalid_credentials")
			return
		}
		for t := range s.UserTokens.Get(u.ID) {
			a.put("session", tokenChange{t, ""})
		}
		token := id()
		a.put("session", tokenChange{token, u.ID})
		setMeasurementIdentity(r, u.ID)
		if isInternal(r) {
			w.Header().Set("X-ISU-Identity", u.ID)
		}
		output(w, 200, object{"token": token, "user": sessionUser(u)})
		return
	}
	if path == "/api/admin/auth/login" && method == "POST" {
		var v struct{ Username, Password string }
		decode(r, &v)
		u := s.Admins.Get(v.Username)
		if !usernameRE.MatchString(v.Username) || u == nil || !hmac.Equal([]byte(u.Password), []byte(secretHash(v.Password))) {
			failure(w, 401, "invalid_credentials")
			return
		}
		t := id()
		a.put("adminsession", tokenChange{t, u.ID})
		output(w, 200, object{"token": t, "administratorId": u.ID})
		return
	}
	if strings.HasPrefix(path, "/api/admin/") {
		if !a.admin(w, r) {
			return
		}
		a.adminAPI(w, r)
		return
	}
	if strings.HasPrefix(path, "/api/images/") && method == "GET" {
		a.getImage(w, r, strings.TrimPrefix(path, "/api/images/"))
		return
	}
	region := !(path == "/api/auth/logout" || path == "/api/me" && method == "PATCH" || path == "/api/me/profile-image")
	u := a.auth(w, r, region)
	if u == nil {
		return
	}
	switch {
	case path == "/api/auth/logout" && method == "POST":
		a.put("session", tokenChange{strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ""})
		w.WriteHeader(204)
	case path == "/api/me" && method == "GET":
		output(w, 200, a.me(u))
	case path == "/api/me" && method == "PATCH":
		var v struct{ DisplayName, Bio, Region *string }
		if !decode(r, &v) || (v.DisplayName == nil && v.Bio == nil && v.Region == nil) || (v.DisplayName != nil && !validText(*v.DisplayName, 1, 100)) || (v.Bio != nil && !validText(*v.Bio, 0, 500)) || (v.Region != nil && !validRegion(*v.Region)) {
			failure(w, 400, "invalid_profile")
			return
		}
		n := *u
		if v.DisplayName != nil {
			n.DisplayName = *v.DisplayName
		}
		if v.Bio != nil {
			n.Bio = *v.Bio
		}
		if v.Region != nil {
			n.Region = *v.Region
		}
		a.put("account", n)
		if n.Region != u.Region {
			a.notifier.userChanged(u.ID)
		}
		output(w, 200, a.me(&n))
	case path == "/api/me/profile-image":
		if method == "DELETE" {
			if u.ProfileImageID == nil {
				failure(w, 404, "account_not_found")
				return
			}
			n := *u
			n.ProfileImageID = nil
			a.put("account", n)
			w.WriteHeader(204)
		} else if method == "PUT" {
			if r.Header.Get("Content-Type") != "image/jpeg" {
				failure(w, 415, "unsupported_media_type")
				return
			}
			p, _ := r.Context().Value(uploadKey{}).(*preparedUpload)
			if p == nil || !p.ok {
				if p != nil && p.invalidJPEG {
					a.failIP(r, "jpeg", 2, 5*time.Second)
				}
				failure(w, 400, "invalid_profile_image")
				return
			}
			im, e := a.addImage(r, u.ID)
			if e != nil {
				failure(w, 500, "image_storage_failed")
				return
			}
			n := *u
			n.ProfileImageID = &im.ID
			a.put("account", n)
			w.WriteHeader(204)
		} else {
			failure(w, 405, "method_not_allowed")
		}
	case path == "/api/me/account-deletion" && method == "POST":
		n := *u
		n.Status = "deleted"
		a.put("account", n)
		for t := range s.UserTokens.Get(u.ID) {
			a.put("session", tokenChange{t, ""})
		}
		a.notifier.userChanged(u.ID)
		w.WriteHeader(204)
	case path == "/api/users" && method == "GET":
		a.search(w, r, u)
	case strings.HasPrefix(path, "/api/users/") && method == "GET":
		v := s.Accounts.Get(strings.TrimPrefix(path, "/api/users/"))
		if v == nil || v.Status != "active" || s.blocked(u.ID, v.ID) {
			failure(w, 404, "relationship_not_found")
			return
		}
		output(w, 200, a.user(v, v.AccountType == "certified_public" || s.friend(u.ID, v.ID)))
	case path == "/api/posting-window/current" && method == "GET":
		v := s.window(u)
		p, d := s.dayPosts(u)
		var j any
		if v != nil {
			j = windowJSON(v)
		}
		output(w, 200, object{"window": j, "hasPosted": p != nil, "canPost": s.canPost(u), "deletionCount": d})
	case path == "/api/posts" && method == "POST":
		a.createPost(w, r, u)
	case path == "/api/posts/me/current" && method == "GET":
		p, _ := s.dayPosts(u)
		if p == nil {
			failure(w, 404, "post_not_found")
			return
		}
		output(w, 200, a.post(p))
	case strings.HasPrefix(path, "/api/feeds/") && method == "GET":
		a.feed(w, r, u, strings.TrimPrefix(path, "/api/feeds/"))
	case path == "/api/memories" || strings.HasPrefix(path, "/api/memories/"):
		a.memories(w, r, u)
	case path == "/api/me/export" && method == "GET":
		a.export(w, r, u)
	case strings.HasPrefix(path, "/api/posts/"):
		parts := strings.Split(strings.TrimPrefix(path, "/api/posts/"), "/")
		if len(parts) == 2 && (parts[1] == "comments" || parts[1] == "realmojis") {
			a.interactions(w, r, u, parts[0], parts[1])
			return
		}
		if len(parts) == 1 {
			a.postAPI(w, r, u, parts[0])
			return
		}
		failure(w, 404, "not_found")
	case strings.HasPrefix(path, "/api/comments/") && method == "DELETE":
		a.deleteInteraction(w, r, u, "comment", strings.TrimPrefix(path, "/api/comments/"))
	case strings.HasPrefix(path, "/api/realmojis/") && method == "DELETE":
		a.deleteInteraction(w, r, u, "reaction", strings.TrimPrefix(path, "/api/realmojis/"))
	case path == "/api/friend-requests" || strings.HasPrefix(path, "/api/friend-requests/") || strings.HasPrefix(path, "/api/relationships/") || strings.HasPrefix(path, "/api/friendships/") || strings.HasPrefix(path, "/api/follows/") || strings.HasPrefix(path, "/api/blocks/"):
		a.relationshipAPI(w, r, u)
	default:
		failure(w, 404, "not_found")
	}
}
func (a *App) adminAPI(w http.ResponseWriter, r *http.Request) {
	s := a.s
	path := r.URL.Path
	if path == "/api/admin/auth/logout" && r.Method == "POST" {
		a.put("adminsession", tokenChange{strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ""})
		w.WriteHeader(204)
		return
	}
	if strings.HasPrefix(path, "/api/admin/users/") && strings.HasSuffix(path, "/certification") {
		uid := strings.TrimSuffix(strings.TrimPrefix(path, "/api/admin/users/"), "/certification")
		u := s.Accounts.Get(uid)
		if r.Method == "DELETE" {
			failure(w, 409, "certification_irreversible")
			return
		}
		if r.Method != "PUT" {
			failure(w, 405, "method_not_allowed")
			return
		}
		if u == nil || u.Status != "active" || u.AccountType != "regular" {
			failure(w, 409, "certification_conflict")
			return
		}
		v := *u
		v.AccountType = "certified_public"
		a.put("account", v)
		w.WriteHeader(204)
		return
	}
	if path == "/api/admin/posting-window" && r.Method == "POST" {
		var v struct {
			Region, LocalDate string
			OnTimeSeconds     *int64
		}
		if !decode(r, &v) || !validRegion(v.Region) {
			failure(w, 400, "invalid_posting_window")
			return
		}
		d, e := time.Parse("2006-01-02", v.LocalDate)
		if e != nil || d.Format("2006-01-02") != v.LocalDate {
			failure(w, 400, "invalid_posting_window")
			return
		}
		for _, x := range s.Windows.All() {
			if x.Region == v.Region && x.LocalDate >= v.LocalDate {
				failure(w, 409, "posting_window_conflict")
				return
			}
		}
		now := time.Now().UTC()
		seconds := int64(86400)
		if v.OnTimeSeconds != nil {
			seconds = *v.OnTimeSeconds
		}
		if old := s.Windows.Get(s.Current.Get(v.Region)); old != nil {
			c := *old
			c.EndedAt = &now
			a.put("window", c)
		}
		n := Window{ID: id(), Region: v.Region, LocalDate: v.LocalDate, StartsAt: now, Deadline: now.Add(time.Duration(seconds) * time.Second)}
		a.put("window", n)
		for _, u := range s.Accounts.All() {
			if u.Status == "active" && u.Region == v.Region {
				a.put("notification", Notification{id(), u.ID, n.ID, now})
			}
		}
		a.notifier.regionChanged(v.Region)
		output(w, 201, windowJSON(&n))
		return
	}
	failure(w, 404, "not_found")
}
func (a *App) search(w http.ResponseWriter, r *http.Request, u *Account) {
	q := r.URL.Query()
	term := q.Get("q")
	if !validText(term, 1, 100) {
		failure(w, 400, "invalid_relationship")
		return
	}
	bu, bi := q.Get("beforeUsername"), q.Get("beforeId")
	if (bu == "") != (bi == "") {
		failure(w, 400, "invalid_pagination")
		return
	}
	term = foldSearch(term)
	var us []*Account
	for _, x := range a.s.Accounts.All() {
		if x.ID == u.ID || x.Status != "active" || (bi != "" && (x.Username < bu || x.Username == bu && x.ID <= bi)) {
			continue
		}
		if (strings.Contains(x.searchUsername, term) || strings.Contains(x.searchDisplay, term)) && !a.s.blocked(u.ID, x.ID) {
			us = append(us, x)
		}
	}
	sort.Slice(us, func(i, j int) bool {
		if us[i].Username == us[j].Username {
			return us[i].ID < us[j].ID
		}
		return us[i].Username < us[j].Username
	})
	items := []object{}
	for _, x := range us {
		items = append(items, a.user(x, x.AccountType == "certified_public"))
		if len(items) == 51 {
			break
		}
	}
	page(w, items)
}
func (a *App) addImage(r *http.Request, uid string) (*Image, error) {
	p, ok := r.Context().Value(uploadKey{}).(*preparedUpload)
	if !ok || p.fileID == "" {
		return nil, errors.New("upload was not staged")
	}
	if p.storageErr != nil {
		return nil, p.storageErr
	}
	im := &Image{ID: p.fileID, OwnerID: uid, MediaType: "image/jpeg", Size: p.size}
	a.put("image", im)
	p.committed = true
	return im, nil
}
func (a *App) validImage(im *Image) bool {
	s := a.s
	if im == nil || !s.active(im.OwnerID) {
		return false
	}
	u := s.Accounts.Get(im.OwnerID)
	if u.ProfileImageID != nil && *u.ProfileImageID == im.ID {
		return true
	}
	if im.Kind == 'p' {
		p := s.Posts.get(im.Ref)
		return p != nil && p.DeletedAt == nil
	}
	if im.Kind == 'r' {
		v := s.Reactions.get(im.Ref)
		if v == nil || v.DeletedAt != nil || v.ReplacedAt != nil {
			return false
		}
		p := s.Posts.get(v.PostID)
		return p != nil && p.DeletedAt == nil && s.active(p.AuthorID)
	}
	return false
}
func (a *App) getImage(w http.ResponseWriter, r *http.Request, i string) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	es, ss := q["exp"], q["sig"]
	if len(es) != 1 || len(ss) != 1 {
		failure(w, 403, "image_forbidden")
		return
	}
	exp, e := strconv.ParseInt(es[0], 10, 64)
	raw, e2 := base64.RawURLEncoding.DecodeString(ss[0])
	im := a.s.Images.get(i)
	if e != nil || strconv.FormatInt(exp, 10) != es[0] || exp <= time.Now().Unix() || e2 != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != ss[0] || im == nil || !a.s.active(im.OwnerID) {
		failure(w, 403, "image_forbidden")
		return
	}
	copyImage, valid := *im, a.validImage(im)
	secret := a.s.Accounts.Get(im.OwnerID).Secret
	responseWork(w, func(w http.ResponseWriter) {
		if !hmac.Equal([]byte(ss[0]), []byte(a.signImage(i, secret, exp))) {
			a.mu.RLock()
			stripe := &a.transactions[stateHash("ip:"+ipOf(r))%stateShardCount]
			stripe.Lock()
			a.globalGate.RLock()
			a.failIP(r, "signature", 3, 5*time.Second)
			a.commit()
			a.globalGate.RUnlock()
			stripe.Unlock()
			a.mu.RUnlock()
			failure(w, 403, "image_forbidden")
			return
		}
		if !valid {
			failure(w, 403, "image_forbidden")
			return
		}
		f, off, e := a.imageReader(&copyImage)
		if e != nil {
			failure(w, 500, "image_unavailable")
			return
		}
		defer f.Close()
		// Signature work and opening storage can wait. Recheck immediately
		// before publishing a cacheable response or evaluating its validator.
		a.lockSnapshot()
		current := a.s.Images.get(i)
		cacheable := a.validImage(current)
		checkedAt := time.Now()
		a.unlockSnapshot()
		if !cacheable || exp <= checkedAt.Unix() {
			failure(w, 403, "image_forbidden")
			return
		}
		w.Header().Set("Date", checkedAt.UTC().Format(http.TimeFormat))
		if copyImage.Kind == 'p' {
			maxAge := min(int64(5), exp-checkedAt.Unix())
			w.Header().Set("Cache-Control", "private, max-age="+strconv.FormatInt(maxAge, 10)+", must-revalidate")
		} else {
			// Image bytes are immutable for an ID; replacement creates a new ID.
			etag := strconv.Quote("image-" + copyImage.ID)
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "private, no-cache")
			if imageETagMatches(r.Header.Values("If-None-Match"), etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.Header().Set("Content-Type", copyImage.MediaType)
		w.Header().Set("Content-Length", strconv.FormatInt(copyImage.Size, 10))
		copyStream(w, io.NewSectionReader(f, off, copyImage.Size))
	})
}

func imageETagMatches(values []string, etag string) bool {
	for _, value := range values {
		for _, candidate := range strings.Split(value, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
				return true
			}
		}
	}
	return false
}

func multipartImage(r *http.Request, max int) ([]byte, bool) {
	if p, ok := r.Context().Value(uploadKey{}).(*preparedUpload); ok {
		return p.data, p.ok
	}
	b, ok := readMultipartImage(r)
	if !ok {
		return nil, false
	}
	cfg, e := jpeg.DecodeConfig(bytes.NewReader(b))
	if e != nil || cfg.Width > max || cfg.Height > max {
		return nil, false
	}
	if !validateJPEGEntropy() {
		return b, true
	}
	if max > 512 {
		if !validateLargeJPEG(r.Context(), b) {
			return nil, false
		}
	} else if e = jpegvalidate.Validate(bytes.NewReader(b)); e != nil {
		return nil, false
	}
	return b, true
}

// Read the selected file once. ParseMultipartForm would retain a second copy
// before callers read the file back. Preserve its query/form value ordering.
func readMultipartImage(r *http.Request) ([]byte, bool) {
	var image []byte
	found := readMultipartSelected(r, func(part io.Reader) error {
		var err error
		image, err = io.ReadAll(part)
		return err
	})
	return image, found
}

func readMultipartSelected(r *http.Request, consume func(io.Reader) error) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return false
	}
	form := &multipart.Form{Value: make(map[string][]string)}
	found := false
	for n := 0; ; n++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil || n >= 1000 {
			return false
		}
		name := part.FormName()
		if name == "" || part.FileName() != "" && (name != "image" || found) {
			_, err = io.Copy(io.Discard, part)
		} else if part.FileName() == "" {
			var data []byte
			data, err = io.ReadAll(part)
			form.Value[name] = append(form.Value[name], string(data))
		} else {
			err = consume(part)
			found = true
		}
		part.Close()
		if err != nil {
			return false
		}
	}
	for key, values := range form.Value {
		r.Form[key] = append(r.Form[key], values...)
		r.PostForm[key] = append(r.PostForm[key], values...)
	}
	r.MultipartForm = form
	return found
}
func (a *App) createPost(w http.ResponseWriter, r *http.Request, u *Account) {
	_, ok := multipartImage(r, 4096)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	caption, vis := r.FormValue("caption"), r.FormValue("visibility")
	loc := Location{Kind: r.FormValue("location_kind")}
	lat, lon, local := r.FormValue("latitude"), r.FormValue("longitude"), r.FormValue("locality")
	valid := ok && validText(caption, 0, 1000) && validVisibility(vis)
	switch loc.Kind {
	case "none":
		valid = valid && lat == "" && lon == "" && local == ""
	case "approximate":
		valid = valid && lat == "" && lon == "" && validText(local, 1, 100)
		loc.Locality = &local
	case "exact":
		x, e := strconv.ParseFloat(lat, 64)
		y, f := strconv.ParseFloat(lon, 64)
		valid = valid && e == nil && f == nil && !math.IsNaN(x) && !math.IsNaN(y) && x >= -90 && x <= 90 && y >= -180 && y <= 180 && local == ""
		loc.Latitude = &x
		loc.Longitude = &y
	default:
		valid = false
	}
	if !valid {
		failure(w, 400, "invalid_post")
		return
	}
	if !a.s.canPost(u) {
		failure(w, 409, "post_slot_unavailable")
		return
	}
	im, e := a.addImage(r, u.ID)
	if e != nil {
		failure(w, 500, "image_storage_failed")
		return
	}
	p := Post{ID: id(), AuthorID: u.ID, WindowID: a.s.window(u).ID, ImageID: im.ID, Caption: caption, Visibility: vis, Location: loc, PostedAt: time.Now().UTC()}
	a.put("post", p)
	output(w, 201, a.post(&p))
}
func (a *App) postAPI(w http.ResponseWriter, r *http.Request, u *Account, i string) {
	p := a.s.Posts.get(i)
	if p == nil || p.DeletedAt != nil {
		failure(w, 404, "post_not_found")
		return
	}
	if r.Method == "GET" {
		if !a.s.view(u, p) {
			failure(w, 404, "post_not_found")
			return
		}
		output(w, 200, a.post(p))
		return
	}
	if r.Method == "PATCH" {
		var v struct{ Caption, Visibility *string }
		if !decode(r, &v) || (v.Caption == nil && v.Visibility == nil) || (v.Caption != nil && !validText(*v.Caption, 0, 1000)) || (v.Visibility != nil && !validVisibility(*v.Visibility)) {
			failure(w, 400, "invalid_post")
			return
		}
		if p.AuthorID != u.ID || a.s.Windows.Get(p.WindowID).EndedAt != nil {
			failure(w, 403, "post_not_editable")
			return
		}
		n := *p
		if v.Caption != nil {
			n.Caption = *v.Caption
		}
		if v.Visibility != nil {
			n.Visibility = *v.Visibility
		}
		a.put("post", n)
		output(w, 200, a.post(&n))
		return
	}
	if r.Method == "DELETE" {
		if p.AuthorID != u.ID {
			failure(w, 403, "post_not_editable")
			return
		}
		date := a.s.Windows.Get(p.WindowID).LocalDate
		n := 0
		for _, pid := range a.s.UserPosts.Get(u.ID) {
			x := a.s.Posts.get(pid)
			if x.DeletedAt != nil && a.s.Windows.Get(x.WindowID).LocalDate == date {
				n++
			}
		}
		if n >= 2 {
			failure(w, 409, "post_slot_unavailable")
			return
		}
		x := *p
		now := time.Now().UTC()
		x.DeletedAt = &now
		a.put("post", x)
		w.WriteHeader(204)
		return
	}
	failure(w, 405, "method_not_allowed")
}
