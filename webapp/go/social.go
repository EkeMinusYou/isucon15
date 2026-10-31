package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (a *App) listComments(pid string) []*Comment {
	out := []*Comment{}
	for _, i := range a.s.PostComments[pid] {
		c := a.s.Comments.get(i)
		if c.DeletedAt == nil && a.s.active(c.AuthorID) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return after(out[j].CreatedAt, out[j].ID, out[i].CreatedAt, out[i].ID) })
	return out
}
func (a *App) listReactions(pid string) []*Reaction {
	out := []*Reaction{}
	for _, i := range a.s.PostReactions[pid] {
		c := a.s.Reactions.get(i)
		if c.DeletedAt == nil && c.ReplacedAt == nil && a.s.active(c.ReactorID) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return before(out[j].CreatedAt, out[j].ID, out[i].CreatedAt, out[i].ID) })
	return out
}
func (a *App) interactions(w http.ResponseWriter, r *http.Request, u *Account, pid, kind string) {
	s := a.s
	p := s.Posts.get(pid)
	allowed := s.view(u, p)
	if kind == "comments" {
		allowed = s.interact(u, p)
	}
	if !allowed || (kind == "realmojis" && r.Method == "POST" && p.AuthorID == u.ID) {
		failure(w, 403, "interaction_forbidden")
		return
	}
	if r.Method == "POST" {
		if kind == "comments" {
			var v struct{ Body string }
			if !decode(r, &v) || !validText(v.Body, 1, 1000) {
				failure(w, 400, "invalid_interaction")
				return
			}
			c := Comment{ID: id(), PostID: pid, AuthorID: u.ID, Body: v.Body, CreatedAt: time.Now().UTC()}
			a.put("comment", c)
			output(w, 201, a.comment(&c))
		} else {
			b, ok := multipartImage(r, 512)
			if r.MultipartForm != nil {
				defer r.MultipartForm.RemoveAll()
			}
			t := r.FormValue("reactionType")
			valid := false
			for _, x := range []string{"like", "smile", "surprise", "heart", "laugh", "instant"} {
				valid = valid || t == x
			}
			if !ok || !valid {
				failure(w, 400, "invalid_realmoji")
				return
			}
			im, e := a.addImage(u.ID, b)
			if e != nil {
				failure(w, 500, "image_storage_failed")
				return
			}
			now := time.Now().UTC()
			for _, i := range s.PostReactions[pid] {
				old := s.Reactions.get(i)
				if old.ReactorID == u.ID && old.ReplacedAt == nil && old.DeletedAt == nil {
					v := *old
					v.ReplacedAt = &now
					a.put("reaction", v)
				}
			}
			v := Reaction{ID: id(), PostID: pid, ReactorID: u.ID, ImageID: im.ID, Type: t, CreatedAt: now}
			a.put("reaction", v)
			output(w, 201, a.reaction(&v))
		}
		return
	}
	if r.Method != "GET" {
		failure(w, 405, "method_not_allowed")
		return
	}
	items := []object{}
	if kind == "comments" {
		ct, ci, ok := pageCursor(w, r, "afterCreatedAt", "afterId")
		if !ok {
			return
		}
		for _, v := range a.listComments(pid) {
			if after(v.CreatedAt, v.ID, ct, ci) {
				items = append(items, a.comment(v))
				if len(items) == 51 {
					break
				}
			}
		}
	} else {
		ct, ci, ok := pageCursor(w, r, "beforeCreatedAt", "beforeId")
		if !ok {
			return
		}
		for _, v := range a.listReactions(pid) {
			if before(v.CreatedAt, v.ID, ct, ci) {
				items = append(items, a.reaction(v))
				if len(items) == 51 {
					break
				}
			}
		}
	}
	page(w, items)
}
func (a *App) deleteInteraction(w http.ResponseWriter, r *http.Request, u *Account, kind, i string) {
	now := time.Now().UTC()
	if kind == "comment" {
		v := a.s.Comments.get(i)
		if v == nil || v.DeletedAt != nil {
			failure(w, 404, "interaction_not_found")
			return
		}
		if v.AuthorID != u.ID && a.s.Posts.get(v.PostID).AuthorID != u.ID {
			failure(w, 403, "interaction_forbidden")
			return
		}
		c := *v
		c.DeletedAt = &now
		a.put(kind, c)
	} else {
		v := a.s.Reactions.get(i)
		if v == nil || v.DeletedAt != nil {
			failure(w, 404, "interaction_not_found")
			return
		}
		if v.ReactorID != u.ID && a.s.Posts.get(v.PostID).AuthorID != u.ID {
			failure(w, 403, "interaction_forbidden")
			return
		}
		c := *v
		c.DeletedAt = &now
		a.put(kind, c)
	}
	w.WriteHeader(204)
}
func (a *App) score(uid string, p *Post) int {
	s := a.s
	engaged := map[string]bool{}
	for _, x := range s.PostComments[p.ID] {
		v := s.Comments.get(x)
		if v.DeletedAt == nil && s.active(v.AuthorID) && s.friend(uid, v.AuthorID) {
			engaged[v.AuthorID] = true
		}
	}
	for _, x := range s.PostReactions[p.ID] {
		v := s.Reactions.get(x)
		if v.DeletedAt == nil && v.ReplacedAt == nil && s.active(v.ReactorID) && s.friend(uid, v.ReactorID) {
			engaged[v.ReactorID] = true
		}
	}
	c := 0
	for x := range s.Adj[p.AuthorID] {
		c += s.common(uid, x)
	}
	if c > 30 {
		c = 30
	}
	return 10*len(engaged) + 3*s.common(uid, p.AuthorID) + c
}
func (a *App) feed(w http.ResponseWriter, r *http.Request, u *Account, kind string) {
	if kind != "friends" && kind != "following" && kind != "public" && kind != "friends-of-friends" {
		failure(w, 400, "invalid_post")
		return
	}
	ct, ci, ok := pageCursor(w, r, "beforePostedAt", "beforeId")
	if !ok {
		return
	}
	cs := 0
	if kind == "friends-of-friends" {
		sc := r.URL.Query().Get("beforeScore")
		if (ci == "") != (sc == "") {
			failure(w, 400, "invalid_pagination")
			return
		}
		if sc != "" {
			var e error
			cs, e = strconv.Atoi(sc)
			if e != nil {
				failure(w, 400, "invalid_pagination")
				return
			}
		}
	}
	type entry struct {
		p *Post
		s int
	}
	xs := []entry{}
	s := a.s
	// Only the active windows can contribute visible posts, independent of history size.
	for aid := range s.LivePosts {
		author := s.Accounts[aid]
		if author.ID == u.ID || author.Status != "active" {
			continue
		}
		match := kind == "public" || kind == "friends" && s.friend(u.ID, author.ID) || kind == "following" && s.Follows[directed(u.ID, author.ID)] != nil || kind == "friends-of-friends" && !s.friend(u.ID, author.ID) && s.common(u.ID, author.ID) > 0
		if !match {
			continue
		}
		for _, pid := range s.LivePosts[author.ID] {
			p := s.Posts.get(pid)
			if s.Windows[p.WindowID].EndedAt != nil || !s.view(u, p) || kind == "public" && p.Visibility != "public" {
				continue
			}
			score := 0
			if kind == "friends-of-friends" {
				score = a.score(u.ID, p)
			}
			if ci != "" {
				if kind == "friends-of-friends" {
					if score > cs || score == cs && !before(p.PostedAt, p.ID, ct, ci) {
						continue
					}
				} else if !before(p.PostedAt, p.ID, ct, ci) {
					continue
				}
			}
			xs = append(xs, entry{p, score})
		}
	}
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].s != xs[j].s {
			return xs[i].s > xs[j].s
		}
		return before(xs[j].p.PostedAt, xs[j].p.ID, xs[i].p.PostedAt, xs[i].p.ID)
	})
	items := []object{}
	for _, x := range xs {
		v := a.post(x.p)
		if kind == "friends-of-friends" {
			v["score"] = x.s
		}
		items = append(items, v)
		if len(items) == 51 {
			break
		}
	}
	page(w, items)
}
func (a *App) memories(w http.ResponseWriter, r *http.Request, u *Account) {
	if r.Method != "GET" {
		failure(w, 405, "method_not_allowed")
		return
	}
	key := r.URL.Query().Get("month")
	layout := "2006-01"
	if r.URL.Path != "/api/memories" {
		key = strings.TrimPrefix(r.URL.Path, "/api/memories/")
		layout = "2006-01-02"
	}
	t, e := time.Parse(layout, key)
	if e != nil || t.Format(layout) != key {
		failure(w, 400, "invalid_memory_period")
		return
	}
	ct, ci, ok := pageCursor(w, r, "beforePostedAt", "beforeId")
	if !ok {
		return
	}
	ps := a.ownPosts(u, false)
	items := []object{}
	for _, p := range ps {
		if !strings.HasPrefix(a.s.Windows[p.WindowID].LocalDate, key) || !before(p.PostedAt, p.ID, ct, ci) {
			continue
		}
		v := a.post(p)
		cs := []object{}
		rs := []object{}
		for _, c := range a.listComments(p.ID) {
			cs = append(cs, a.comment(c))
			if len(cs) == 50 {
				break
			}
		}
		for _, c := range a.listReactions(p.ID) {
			rs = append(rs, a.reaction(c))
			if len(rs) == 50 {
				break
			}
		}
		v["comments"] = cs
		v["realmojis"] = rs
		items = append(items, v)
		if len(items) == 51 {
			break
		}
	}
	page(w, items)
}
func (a *App) ownPosts(u *Account, asc bool) []*Post {
	ps := []*Post{}
	for _, i := range a.s.UserPosts[u.ID] {
		p := a.s.Posts.get(i)
		if p.DeletedAt == nil {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool {
		if asc {
			return before(ps[i].PostedAt, ps[i].ID, ps[j].PostedAt, ps[j].ID)
		}
		return before(ps[j].PostedAt, ps[j].ID, ps[i].PostedAt, ps[i].ID)
	})
	return ps
}
func (a *App) export(w http.ResponseWriter, r *http.Request, u *Account) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	items := []object{}
	for n, p := range a.ownPosts(u, true) {
		v := a.post(p)
		name := fmt.Sprintf("images/%06d.jpg", n+1)
		v["imagePath"] = name
		items = append(items, v)
		f, e := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if e != nil {
			failure(w, 500, "export_failed")
			return
		}
		im := a.s.Images.get(p.ImageID)
		src, off, e := a.imageReader(im)
		if e != nil {
			failure(w, 500, "export_failed")
			return
		}
		_, e = io.Copy(f, io.NewSectionReader(src, off, im.Size))
		src.Close()
		if e != nil {
			failure(w, 500, "export_failed")
			return
		}
	}
	f, e := zw.Create("posts.json")
	if e == nil {
		e = json.NewEncoder(f).Encode(items)
	}
	if e != nil {
		failure(w, 500, "export_failed")
		return
	}
	if e = zw.Close(); e != nil {
		failure(w, 500, "export_failed")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="isureal-posts.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}
func relationJSON(v *Relation) object {
	return object{"relationId": v.ID, "status": v.Status, "updatedAt": v.UpdatedAt}
}
func (a *App) establish(v *Relation) {
	now := v.UpdatedAt
	a.put("friend", Relation{ID: id(), User1ID: v.RequesterID, User2ID: v.RecipientID, CreatedAt: now, UpdatedAt: now})
}
func (a *App) relationshipAPI(w http.ResponseWriter, r *http.Request, u *Account) {
	s := a.s
	p := r.URL.Path
	now := time.Now().UTC()
	if strings.HasPrefix(p, "/api/relationships/") {
		a.relationships(w, r, u, strings.TrimPrefix(p, "/api/relationships/"))
		return
	}
	if p == "/api/friend-requests" && r.Method == "POST" {
		var v struct{ RecipientID string }
		if !decode(r, &v) || v.RecipientID == "" || v.RecipientID == u.ID {
			failure(w, 400, "invalid_relationship")
			return
		}
		if !s.active(v.RecipientID) {
			failure(w, 404, "relationship_not_found")
			return
		}
		if s.blocked(u.ID, v.RecipientID) || s.friend(u.ID, v.RecipientID) {
			failure(w, 409, "relationship_conflict")
			return
		}
		var reverse *Relation
		for _, q := range s.Requests {
			if q.Status != "pending" {
				continue
			}
			if q.RequesterID == u.ID && q.RecipientID == v.RecipientID {
				failure(w, 409, "relationship_conflict")
				return
			}
			if q.RequesterID == v.RecipientID && q.RecipientID == u.ID {
				reverse = q
			}
		}
		if reverse != nil {
			q := *reverse
			q.Status = "accepted"
			q.UpdatedAt = now
			a.put("request", q)
			a.establish(&q)
			output(w, 201, relationJSON(&q))
			return
		}
		q := Relation{ID: id(), RequesterID: u.ID, RecipientID: v.RecipientID, Status: "pending", CreatedAt: now, UpdatedAt: now}
		a.put("request", q)
		output(w, 201, relationJSON(&q))
		return
	}
	if strings.HasPrefix(p, "/api/friend-requests/") && r.Method == "POST" {
		parts := strings.Split(strings.TrimPrefix(p, "/api/friend-requests/"), "/")
		if len(parts) != 2 {
			failure(w, 404, "relationship_not_found")
			return
		}
		v := s.Requests[parts[0]]
		action := parts[1]
		if v == nil {
			failure(w, 404, "relationship_not_found")
			return
		}
		if !s.active(v.RequesterID) || !s.active(v.RecipientID) {
			failure(w, 404, "relationship_not_found")
			return
		}
		if v.Status != "pending" || !((action == "accept" || action == "reject") && v.RecipientID == u.ID || action == "cancel" && v.RequesterID == u.ID) {
			failure(w, 403, "relationship_action_forbidden")
			return
		}
		if action == "accept" && s.blocked(v.RequesterID, v.RecipientID) {
			failure(w, 409, "relationship_conflict")
			return
		}
		q := *v
		q.UpdatedAt = now
		switch action {
		case "accept":
			q.Status = "accepted"
		case "reject":
			q.Status = "rejected"
		case "cancel":
			q.Status = "cancelled"
		}
		a.put("request", q)
		if action == "accept" {
			a.establish(&q)
		}
		output(w, 200, relationJSON(&q))
		return
	}
	parts := strings.Split(strings.TrimPrefix(p, "/api/"), "/")
	if len(parts) != 2 {
		failure(w, 404, "not_found")
		return
	}
	kind, uid := parts[0], parts[1]
	if uid == "" || uid == u.ID {
		failure(w, 400, "invalid_relationship")
		return
	}
	if !s.active(uid) {
		failure(w, 404, "relationship_not_found")
		return
	}
	switch kind {
	case "friendships":
		if r.Method != "DELETE" {
			failure(w, 405, "method_not_allowed")
			return
		}
		if !s.friend(u.ID, uid) {
			failure(w, 409, "relationship_conflict")
			return
		}
		a.put("unfriend", [2]string{u.ID, uid})
	case "follows":
		if r.Method == "PUT" {
			if s.Follows[directed(u.ID, uid)] != nil || s.Accounts[uid].AccountType != "certified_public" || s.blocked(u.ID, uid) {
				failure(w, 409, "relationship_conflict")
				return
			}
			a.put("follow", Relation{ID: id(), FollowerID: u.ID, FolloweeID: uid, CreatedAt: now, UpdatedAt: now})
		} else if r.Method == "DELETE" {
			if s.Follows[directed(u.ID, uid)] == nil {
				failure(w, 409, "relationship_conflict")
				return
			}
			a.put("unfollow", [2]string{u.ID, uid})
		} else {
			failure(w, 405, "method_not_allowed")
			return
		}
	case "blocks":
		if r.Method == "PUT" {
			if s.Blocks[directed(u.ID, uid)] != nil {
				failure(w, 409, "relationship_conflict")
				return
			}
			a.put("block", Relation{ID: id(), BlockerID: u.ID, BlockedID: uid, CreatedAt: now})
			if s.friend(u.ID, uid) {
				a.put("unfriend", [2]string{u.ID, uid})
			}
			a.put("unfollow", [2]string{u.ID, uid})
			a.put("unfollow", [2]string{uid, u.ID})
			for _, v := range s.Requests {
				if v.Status == "pending" && (v.RequesterID == u.ID && v.RecipientID == uid || v.RequesterID == uid && v.RecipientID == u.ID) {
					q := *v
					q.Status = "cancelled"
					q.UpdatedAt = now
					a.put("request", q)
				}
			}
		} else if r.Method == "DELETE" {
			if s.Blocks[directed(u.ID, uid)] == nil {
				failure(w, 403, "relationship_action_forbidden")
				return
			}
			a.put("unblock", [2]string{u.ID, uid})
		} else {
			failure(w, 405, "method_not_allowed")
			return
		}
	default:
		failure(w, 404, "not_found")
		return
	}
	w.WriteHeader(204)
}
func (a *App) relationships(w http.ResponseWriter, r *http.Request, u *Account, kind string) {
	if r.Method != "GET" {
		failure(w, 405, "method_not_allowed")
		return
	}
	ct, ci, ok := pageCursor(w, r, "beforeUpdatedAt", "beforeId")
	if !ok {
		return
	}
	type entry struct {
		v   *Relation
		uid string
	}
	xs := []entry{}
	s := a.s
	add := func(v *Relation, uid string) {
		if s.active(uid) && before(v.UpdatedAt, v.ID, ct, ci) {
			xs = append(xs, entry{v, uid})
		}
	}
	switch kind {
	case "friends":
		for uid := range s.Adj[u.ID] {
			add(s.Friends[pair(u.ID, uid)], uid)
		}
	case "following", "followers":
		for _, v := range s.Follows {
			if kind == "following" && v.FollowerID == u.ID {
				add(v, v.FolloweeID)
			}
			if kind == "followers" && v.FolloweeID == u.ID {
				add(v, v.FollowerID)
			}
		}
	case "incoming-requests", "outgoing-requests":
		for _, v := range s.Requests {
			if v.Status != "pending" {
				continue
			}
			if kind == "incoming-requests" && v.RecipientID == u.ID {
				add(v, v.RequesterID)
			}
			if kind == "outgoing-requests" && v.RequesterID == u.ID {
				add(v, v.RecipientID)
			}
		}
	default:
		failure(w, 400, "invalid_relationship")
		return
	}
	sort.Slice(xs, func(i, j int) bool { return before(xs[j].v.UpdatedAt, xs[j].v.ID, xs[i].v.UpdatedAt, xs[i].v.ID) })
	items := []object{}
	for _, x := range xs {
		status := kind
		if strings.HasSuffix(kind, "requests") {
			status = "pending"
		}
		other := s.Accounts[x.uid]
		items = append(items, object{"relationId": x.v.ID, "user": a.user(other, kind == "friends" || other.AccountType == "certified_public"), "status": status, "updatedAt": x.v.UpdatedAt})
		if len(items) == 51 {
			break
		}
	}
	page(w, items)
}
func (a *App) notifications(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	a.mu.Lock()
	if started.Before(a.s.Bans[ipOf(r)]) {
		a.mu.Unlock()
		failure(w, 403, "ip_banned")
		return
	}
	u := a.auth(w, r, true)
	if u == nil {
		a.mu.Unlock()
		return
	}
	uid, region := u.ID, u.Region
	signal := a.signal
	a.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	for {
		a.mu.Lock()
		s := a.s
		u = s.Accounts[uid]
		if u == nil || u.Status != "active" || u.Region != region {
			a.mu.Unlock()
			return
		}
		var selected *Notification
		for _, v := range s.Notifications[uid] {
			win := s.Windows[v.WindowID]
			if v.CreatedAt.After(started) && win != nil && win.EndedAt == nil && (selected == nil || before(v.CreatedAt, v.ID, selected.CreatedAt, selected.ID)) {
				c := v
				selected = &c
			}
		}
		signal = a.signal
		if selected != nil {
			v := object{"id": selected.ID, "postingWindowId": selected.WindowID, "region": s.Windows[selected.WindowID].Region, "createdAt": selected.CreatedAt}
			b, _ := json.Marshal(v)
			a.mu.Unlock()
			fmt.Fprintf(w, "data: %s\n\n", b)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		a.mu.Unlock()
		select {
		case <-signal:
		case <-r.Context().Done():
			return
		}
	}
}
