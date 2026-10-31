package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image/jpeg"
	"io"
	"math"
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

func output(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
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
	xs := a.failures[key][:0]
	for _, t := range a.failures[key] {
		if now.Sub(t) < period {
			xs = append(xs, t)
		}
	}
	xs = append(xs, now)
	if len(xs) >= limit {
		until := now.Add(2 * time.Minute)
		if a.s.Bans[ipOf(r)].Before(until) {
			a.put("ban", banChange{ipOf(r), until})
		}
		xs = nil
	}
	a.failures[key] = xs
}
func (a *App) auth(w http.ResponseWriter, r *http.Request, region bool) *Account {
	t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	u := a.s.Accounts[a.s.Sessions[t]]
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
	t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if a.s.AdminSessions[t] == "" {
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
	key := im.ID + ":" + strconv.FormatInt(exp, 10)
	if a.signatureSecond != exp {
		a.signatures = map[string]string{}
		a.signatureSecond = exp
	}
	if v := a.signatures[key]; v != "" {
		return v
	}
	u := a.s.Accounts[im.OwnerID]
	k, _ := hex.DecodeString(u.Secret)
	mac := hmac.New(sha256.New, k)
	v := []byte(strconv.FormatInt(exp, 10) + ":" + im.ID)
	var sum [32]byte
	for i := 0; i < 1000; i++ {
		mac.Reset()
		mac.Write(v)
		v = mac.Sum(sum[:0])
	}
	sig := base64.RawURLEncoding.EncodeToString(v)
	a.signatures[key] = sig
	return sig
}
func (a *App) imageURL(i string) any {
	im := a.s.Images[i]
	if im == nil {
		return nil
	}
	exp := time.Now().Unix() + 86400
	return "/api/images/" + i + "?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + a.signed(im, exp)
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
	w := a.s.Windows[p.WindowID]
	late := int64(p.PostedAt.Sub(w.Deadline) / time.Second)
	if late < 0 {
		late = 0
	}
	return object{"id": p.ID, "author": a.user(a.s.Accounts[p.AuthorID], true), "postingWindowId": p.WindowID, "imageUrl": a.imageURL(p.ImageID), "caption": p.Caption, "visibility": p.Visibility, "location": p.Location, "postedAt": p.PostedAt, "onTime": !p.PostedAt.After(w.Deadline), "lateSeconds": late}
}
func (a *App) comment(c *Comment) object {
	return object{"id": c.ID, "author": a.user(a.s.Accounts[c.AuthorID], true), "body": c.Body, "createdAt": c.CreatedAt}
}
func (a *App) reaction(c *Reaction) object {
	return object{"id": c.ID, "reactor": a.user(a.s.Accounts[c.ReactorID], true), "reactionType": c.Type, "imageUrl": a.imageURL(c.ImageID), "createdAt": c.CreatedAt}
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
	if r.URL.Path == "/api/notifications" && r.Method == "GET" {
		a.notifications(w, r)
		return
	}
	if r.Body != nil {
		b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if e != nil {
			failure(w, 413, "body_too_large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	rr := httptest.NewRecorder()
	a.mu.Lock()
	a.requests++
	if time.Now().Before(a.s.Bans[ipOf(r)]) {
		failure(rr, 403, "ip_banned")
	} else {
		a.dispatch(rr, r)
	}
	a.commit()
	a.mu.Unlock()
	for k, vs := range rr.Header() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
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
		if s.Names[v.Username] != "" {
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
		u := s.Accounts[s.Names[v.Username]]
		if !usernameRE.MatchString(v.Username) || u == nil || u.Status != "active" || !hmac.Equal([]byte(u.Secret), []byte(secretHash(v.SecretKey))) {
			a.failIP(r, "login", 10, 10*time.Second)
			failure(w, 401, "invalid_credentials")
			return
		}
		for t, uid := range s.Sessions {
			if uid == u.ID {
				a.put("session", tokenChange{t, ""})
			}
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
		u := s.Admins[v.Username]
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
		a.wake()
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
			b, _ := io.ReadAll(r.Body)
			cfg, e := jpeg.DecodeConfig(bytes.NewReader(b))
			if e != nil {
				a.failIP(r, "jpeg", 2, 5*time.Second)
				failure(w, 400, "invalid_profile_image")
				return
			}
			if cfg.Width > 512 || cfg.Height > 512 {
				failure(w, 400, "invalid_profile_image")
				return
			}
			if _, e = jpeg.Decode(bytes.NewReader(b)); e != nil {
				a.failIP(r, "jpeg", 2, 5*time.Second)
				failure(w, 400, "invalid_profile_image")
				return
			}
			im, e := a.addImage(u.ID, b)
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
		for t, uid := range s.Sessions {
			if uid == u.ID {
				a.put("session", tokenChange{t, ""})
			}
		}
		a.wake()
		w.WriteHeader(204)
	case path == "/api/users" && method == "GET":
		a.search(w, r, u)
	case strings.HasPrefix(path, "/api/users/") && method == "GET":
		v := s.Accounts[strings.TrimPrefix(path, "/api/users/")]
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
		u := s.Accounts[uid]
		if r.Method == "DELETE" {
			failure(w, 409, "certification_irreversible")
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
		for _, x := range s.Windows {
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
		if old := s.Windows[s.Current[v.Region]]; old != nil {
			c := *old
			c.EndedAt = &now
			a.put("window", c)
		}
		n := Window{ID: id(), Region: v.Region, LocalDate: v.LocalDate, StartsAt: now, Deadline: now.Add(time.Duration(seconds) * time.Second)}
		a.put("window", n)
		for _, u := range s.Accounts {
			if u.Status == "active" && u.Region == v.Region {
				a.put("notification", Notification{id(), u.ID, n.ID, now})
			}
		}
		a.wake()
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
	fold := func(v string) string { return norm.NFC.String(cases.Fold().String(norm.NFC.String(v))) }
	term = fold(term)
	var us []*Account
	for _, x := range a.s.Accounts {
		if x.ID == u.ID || x.Status != "active" || a.s.blocked(u.ID, x.ID) || (bi != "" && (x.Username < bu || x.Username == bu && x.ID <= bi)) {
			continue
		}
		if strings.Contains(fold(x.Username), term) || strings.Contains(fold(x.DisplayName), term) {
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
func (a *App) addImage(uid string, b []byte) (*Image, error) {
	im := &Image{ID: id(), OwnerID: uid, MediaType: "image/jpeg", Size: int64(len(b))}
	p := filepath.Join(a.durable, "images", im.ID)
	if e := os.WriteFile(p, b, 0600); e != nil {
		return nil, e
	}
	a.put("image", im)
	return im, nil
}
func (a *App) validImage(im *Image) bool {
	s := a.s
	if im == nil || !s.active(im.OwnerID) {
		return false
	}
	u := s.Accounts[im.OwnerID]
	if u.ProfileImageID != nil && *u.ProfileImageID == im.ID {
		return true
	}
	if im.Kind == 'p' {
		p := s.Posts[im.Ref]
		return p != nil && p.DeletedAt == nil
	}
	if im.Kind == 'r' {
		v := s.Reactions[im.Ref]
		if v == nil || v.DeletedAt != nil || v.ReplacedAt != nil {
			return false
		}
		p := s.Posts[v.PostID]
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
	im := a.s.Images[i]
	if e != nil || strconv.FormatInt(exp, 10) != es[0] || exp <= time.Now().Unix() || e2 != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != ss[0] || im == nil || !a.s.active(im.OwnerID) {
		failure(w, 403, "image_forbidden")
		return
	}
	if !hmac.Equal([]byte(ss[0]), []byte(a.signed(im, exp))) {
		a.failIP(r, "signature", 3, 5*time.Second)
		failure(w, 403, "image_forbidden")
		return
	}
	if !a.validImage(im) {
		failure(w, 403, "image_forbidden")
		return
	}
	f, off, e := a.imageReader(im)
	if e != nil {
		failure(w, 500, "image_unavailable")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", im.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(im.Size, 10))
	io.Copy(w, io.NewSectionReader(f, off, im.Size))
}
func multipartImage(r *http.Request, max int) ([]byte, bool) {
	if e := r.ParseMultipartForm(32 << 20); e != nil {
		return nil, false
	}
	f, _, e := r.FormFile("image")
	if e != nil {
		return nil, false
	}
	defer f.Close()
	b, e := io.ReadAll(f)
	if e != nil {
		return nil, false
	}
	cfg, e := jpeg.DecodeConfig(bytes.NewReader(b))
	if e != nil || cfg.Width > max || cfg.Height > max {
		return nil, false
	}
	if _, e = jpeg.Decode(bytes.NewReader(b)); e != nil {
		return nil, false
	}
	return b, true
}
func (a *App) createPost(w http.ResponseWriter, r *http.Request, u *Account) {
	b, ok := multipartImage(r, 4096)
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
	im, e := a.addImage(u.ID, b)
	if e != nil {
		failure(w, 500, "image_storage_failed")
		return
	}
	p := Post{ID: id(), AuthorID: u.ID, WindowID: a.s.window(u).ID, ImageID: im.ID, Caption: caption, Visibility: vis, Location: loc, PostedAt: time.Now().UTC()}
	a.put("post", p)
	output(w, 201, a.post(&p))
}
func (a *App) postAPI(w http.ResponseWriter, r *http.Request, u *Account, i string) {
	p := a.s.Posts[i]
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
		if p.AuthorID != u.ID || a.s.Windows[p.WindowID].EndedAt != nil {
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
		date := a.s.Windows[p.WindowID].LocalDate
		n := 0
		for _, pid := range a.s.UserPosts[u.ID] {
			x := a.s.Posts[pid]
			if x.DeletedAt != nil && a.s.Windows[x.WindowID].LocalDate == date {
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
