package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (a *App) listComments(pid string) []*Comment {
	out := []*Comment{}
	for _, i := range a.s.PostComments.Get(pid) {
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
	for _, i := range a.s.PostReactions.Get(pid) {
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
			_, ok := multipartImage(r, 512)
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
			im, e := a.addImage(r, u.ID)
			if e != nil {
				failure(w, 500, "image_storage_failed")
				return
			}
			now := time.Now().UTC()
			for _, i := range s.PostReactions.Get(pid) {
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
			var err error
			cs, err = strconv.Atoi(sc)
			if err != nil {
				failure(w, 400, "invalid_pagination")
				return
			}
		}
	}
	type entry struct {
		post    postSnapshot
		author  string
		engaged []string
		score   int
	}
	xs := []entry{}
	s := a.s
	var ranking *rankingSnapshot
	if kind == "friends-of-friends" {
		ranking = s.snapshotRanking(u.ID)
	}
	for aid, ids := range s.LivePosts.All() {
		author := s.Accounts.Get(aid)
		if author.ID == u.ID || author.Status != "active" {
			continue
		}
		match := kind == "public" || kind == "friends" && s.friend(u.ID, aid) || kind == "following" && s.Follows.Get(directed(u.ID, aid)) != nil || kind == "friends-of-friends" && !s.friend(u.ID, aid) && s.common(u.ID, aid) > 0
		if !match {
			continue
		}
		for _, pid := range ids {
			p := s.Posts.get(pid)
			if s.Windows.Get(p.WindowID).EndedAt != nil || !s.view(u, p) || kind == "public" && p.Visibility != "public" {
				continue
			}
			x := entry{post: a.snapshotPost(p), author: aid}
			if ranking != nil {
				ranking.capture(s, aid)
				for _, id := range ranking.graph[aid] {
					ranking.capture(s, id)
				}
				for _, id := range s.PostComments.Get(pid) {
					c := s.Comments.get(id)
					if c.DeletedAt == nil && s.active(c.AuthorID) {
						x.engaged = append(x.engaged, c.AuthorID)
					}
				}
				for _, id := range s.PostReactions.Get(pid) {
					v := s.Reactions.get(id)
					if v.DeletedAt == nil && v.ReplacedAt == nil && s.active(v.ReactorID) {
						x.engaged = append(x.engaged, v.ReactorID)
					}
				}
			}
			xs = append(xs, x)
		}
	}
	responseWork(w, func(w http.ResponseWriter) {
		for i := range xs {
			if ranking != nil {
				xs[i].score = ranking.score(xs[i].author, xs[i].engaged)
			}
		}
		sort.Slice(xs, func(i, j int) bool {
			if xs[i].score != xs[j].score {
				return xs[i].score > xs[j].score
			}
			return before(xs[j].post.postedAt, xs[j].post.id, xs[i].post.postedAt, xs[i].post.id)
		})
		items := []object{}
		for _, x := range xs {
			if ci != "" && (ranking != nil && (x.score > cs || x.score == cs && !before(x.post.postedAt, x.post.id, ct, ci)) || ranking == nil && !before(x.post.postedAt, x.post.id, ct, ci)) {
				continue
			}
			v := x.post.object()
			if ranking != nil {
				v["score"] = x.score
			}
			items = append(items, v)
			if len(items) == 51 {
				break
			}
		}
		prefetchImageSignatures(r.Context(), items)
		page(w, items)
	})
}
func (a *App) memories(w http.ResponseWriter, r *http.Request, u *Account) {
	if r.Method != "GET" {
		failure(w, 405, "method_not_allowed")
		return
	}
	key, layout := r.URL.Query().Get("month"), "2006-01"
	if r.URL.Path != "/api/memories" {
		key = strings.TrimPrefix(r.URL.Path, "/api/memories/")
		layout = "2006-01-02"
	}
	t, err := time.Parse(layout, key)
	if err != nil || t.Format(layout) != key {
		failure(w, 400, "invalid_memory_period")
		return
	}
	ct, ci, ok := pageCursor(w, r, "beforePostedAt", "beforeId")
	if !ok {
		return
	}
	end := t.AddDate(0, 1, 0)
	if layout == "2006-01-02" {
		end = t.AddDate(0, 0, 1)
	}
	type memory struct {
		post      postSnapshot
		comments  []commentSnapshot
		reactions []reactionSnapshot
	}
	memories := []memory{}
	for _, ref := range a.s.postsInPeriod(u.ID, dateNumber(t.Format("2006-01-02")), dateNumber(end.Format("2006-01-02"))) {
		p := a.s.Posts.get(ref.id)
		if p.DeletedAt != nil {
			continue
		}
		m := memory{post: a.snapshotPost(p)}
		for _, id := range a.s.PostComments.Get(p.ID) {
			c := a.s.Comments.get(id)
			if c.DeletedAt == nil && a.s.active(c.AuthorID) {
				m.comments = append(m.comments, a.snapshotComment(c))
			}
		}
		for _, id := range a.s.PostReactions.Get(p.ID) {
			v := a.s.Reactions.get(id)
			if v.DeletedAt == nil && v.ReplacedAt == nil && a.s.active(v.ReactorID) {
				m.reactions = append(m.reactions, a.snapshotReaction(v))
			}
		}
		memories = append(memories, m)
	}
	responseWork(w, func(w http.ResponseWriter) {
		sort.Slice(memories, func(i, j int) bool {
			return before(memories[j].post.postedAt, memories[j].post.id, memories[i].post.postedAt, memories[i].post.id)
		})
		items := []object{}
		for _, m := range memories {
			if !before(m.post.postedAt, m.post.id, ct, ci) {
				continue
			}
			sort.Slice(m.comments, func(i, j int) bool {
				return after(m.comments[j].createdAt, m.comments[j].id, m.comments[i].createdAt, m.comments[i].id)
			})
			sort.Slice(m.reactions, func(i, j int) bool {
				return before(m.reactions[j].createdAt, m.reactions[j].id, m.reactions[i].createdAt, m.reactions[i].id)
			})
			cs, rs := []object{}, []object{}
			for _, c := range m.comments {
				cs = append(cs, c.object())
				if len(cs) == 50 {
					break
				}
			}
			for _, v := range m.reactions {
				rs = append(rs, v.object())
				if len(rs) == 50 {
					break
				}
			}
			v := m.post.object()
			v["comments"], v["realmojis"] = cs, rs
			items = append(items, v)
			if len(items) == 51 {
				break
			}
		}
		prefetchImageSignatures(r.Context(), items)
		page(w, items)
	})
}
func (a *App) export(w http.ResponseWriter, r *http.Request, u *Account) {
	type entry struct {
		post  postSnapshot
		image Image
	}
	entries := []entry{}
	for _, id := range a.s.UserPosts.Get(u.ID) {
		p := a.s.Posts.get(id)
		if p.DeletedAt != nil {
			continue
		}
		im := a.s.Images.get(p.ImageID)
		if im == nil {
			failure(w, 500, "export_failed")
			return
		}
		entries = append(entries, entry{a.snapshotPost(p), *im})
	}
	responseWork(w, func(w http.ResponseWriter) {
		sort.Slice(entries, func(i, j int) bool {
			return before(entries[i].post.postedAt, entries[i].post.id, entries[j].post.postedAt, entries[j].post.id)
		})
		items, images := []object{}, []Image{}
		for i, e := range entries {
			v := e.post.object()
			v["imagePath"] = fmt.Sprintf("images/%06d.jpg", i+1)
			items = append(items, v)
			images = append(images, e.image)
		}
		prefetchImageSignatures(r.Context(), items)
		metadata, e := json.Marshal(items)
		if e != nil {
			failure(w, 500, "export_failed")
			return
		}
		metadata = append(metadata, '\n')
		type source struct {
			file         *os.File
			offset, size int64
		}
		sources := make([]source, 0, len(images))
		defer func() {
			for _, src := range sources {
				src.file.Close()
			}
		}()
		// Resolve every file before committing ZIP response headers.
		for _, im := range images {
			file, offset, err := a.imageReader(&im)
			if err != nil {
				failure(w, 500, "export_failed")
				return
			}
			sources = append(sources, source{file, offset, im.Size})
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="isureal-posts.zip"`)
		w.Header().Set("Cache-Control", "no-store")
		zw := zip.NewWriter(w)
		for n, src := range sources {
			f, err := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("images/%06d.jpg", n+1), Method: zip.Store})
			if err != nil {
				panic(http.ErrAbortHandler)
			}
			if _, err = io.Copy(f, io.NewSectionReader(src.file, src.offset, src.size)); err != nil {
				panic(http.ErrAbortHandler)
			}
		}
		f, e := zw.Create("posts.json")
		if e == nil {
			_, e = f.Write(metadata)
		}
		if e != nil {
			panic(http.ErrAbortHandler)
		}
		if e = zw.Close(); e != nil {
			panic(http.ErrAbortHandler)
		}
	})
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
		for _, q := range s.RequestOut.Get(u.ID) {
			if q.RecipientID == v.RecipientID {
				failure(w, 409, "relationship_conflict")
				return
			}
		}
		for _, q := range s.RequestIn.Get(u.ID) {
			if q.RequesterID == v.RecipientID {
				reverse = q
				break
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
		v := s.Requests.Get(parts[0])
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
			if s.Follows.Get(directed(u.ID, uid)) != nil || s.Accounts.Get(uid).AccountType != "certified_public" || s.blocked(u.ID, uid) {
				failure(w, 409, "relationship_conflict")
				return
			}
			a.put("follow", Relation{ID: id(), FollowerID: u.ID, FolloweeID: uid, CreatedAt: now, UpdatedAt: now})
		} else if r.Method == "DELETE" {
			if s.Follows.Get(directed(u.ID, uid)) == nil {
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
			if s.Blocks.Get(directed(u.ID, uid)) != nil {
				failure(w, 409, "relationship_conflict")
				return
			}
			a.put("block", Relation{ID: id(), BlockerID: u.ID, BlockedID: uid, CreatedAt: now})
			if s.friend(u.ID, uid) {
				a.put("unfriend", [2]string{u.ID, uid})
			}
			a.put("unfollow", [2]string{u.ID, uid})
			a.put("unfollow", [2]string{uid, u.ID})
			for _, requests := range []map[string]*Relation{s.RequestOut.Get(u.ID), s.RequestIn.Get(u.ID)} {
				for _, v := range requests {
					if v.RequesterID == uid || v.RecipientID == uid {
						q := *v
						q.Status = "cancelled"
						q.UpdatedAt = now
						a.put("request", q)
					}
				}
			}
		} else if r.Method == "DELETE" {
			if s.Blocks.Get(directed(u.ID, uid)) == nil {
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
		for uid := range s.Adj.Get(u.ID) {
			add(s.Friends.Get(pair(u.ID, uid)), uid)
		}
	case "following", "followers":
		for _, v := range s.Follows.All() {
			if kind == "following" && v.FollowerID == u.ID {
				add(v, v.FolloweeID)
			}
			if kind == "followers" && v.FolloweeID == u.ID {
				add(v, v.FollowerID)
			}
		}
	case "incoming-requests", "outgoing-requests":
		requests := s.RequestIn.Get(u.ID)
		if kind == "outgoing-requests" {
			requests = s.RequestOut.Get(u.ID)
		}
		for _, v := range requests {
			if kind == "incoming-requests" {
				add(v, v.RequesterID)
			} else {
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
		other := s.Accounts.Get(x.uid)
		items = append(items, object{"relationId": x.v.ID, "user": a.user(other, kind == "friends" || other.AccountType == "certified_public"), "status": status, "updatedAt": x.v.UpdatedAt})
		if len(items) == 51 {
			break
		}
	}
	page(w, items)
}
func (a *App) notifications(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	a.lockSnapshot()
	if started.Before(a.s.Bans.Get(ipOf(r))) {
		a.unlockSnapshot()
		failure(w, 403, "ip_banned")
		return
	}
	u := a.auth(w, r, true)
	if u == nil {
		a.unlockSnapshot()
		return
	}
	uid, region := u.ID, u.Region
	signal := a.signal
	a.unlockSnapshot()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	for {
		a.lockSnapshot()
		s := a.s
		u = s.Accounts.Get(uid)
		if u == nil || u.Status != "active" || u.Region != region {
			a.unlockSnapshot()
			return
		}
		var selected *Notification
		for _, v := range s.Notifications.Get(uid) {
			win := s.Windows.Get(v.WindowID)
			if v.CreatedAt.After(started) && win != nil && win.EndedAt == nil && (selected == nil || before(v.CreatedAt, v.ID, selected.CreatedAt, selected.ID)) {
				c := v
				selected = &c
			}
		}
		signal = a.signal
		if selected != nil {
			v := object{"id": selected.ID, "postingWindowId": selected.WindowID, "region": s.Windows.Get(selected.WindowID).Region, "createdAt": selected.CreatedAt}
			b, _ := json.Marshal(v)
			a.unlockSnapshot()
			fmt.Fprintf(w, "data: %s\n\n", b)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		a.unlockSnapshot()
		select {
		case <-signal:
		case <-r.Context().Done():
			return
		}
	}
}
