package main

import "time"

type userSnapshot struct {
	id, username, displayName, bio, accountType string
	image                                       any
}

func (a *App) snapshotImage(id, secret string) any {
	// Post, reaction and profile references always belong to their actor.
	// Capture that actor's key without decoding immutable image metadata.
	if !a.s.Images.has(id) {
		return nil
	}
	return imageReference{a, id, secret, time.Now().Unix() + 86400}
}

func (a *App) snapshotUser(u *Account, full bool) userSnapshot {
	v := userSnapshot{id: u.ID, username: u.Username, displayName: u.DisplayName, accountType: u.AccountType}
	if full {
		v.bio = u.Bio
	}
	if u.ProfileImageID != nil {
		v.image = a.snapshotImage(*u.ProfileImageID, u.Secret)
	}
	return v
}
func (u userSnapshot) object() object {
	return object{"id": u.id, "username": u.username, "displayName": u.displayName, "bio": u.bio, "accountType": u.accountType, "profileImageUrl": u.image}
}

type locationSnapshot struct {
	kind, locality                         string
	latitude, longitude                    float64
	hasLatitude, hasLongitude, hasLocality bool
}

func snapshotLocation(v Location) locationSnapshot {
	s := locationSnapshot{kind: v.Kind}
	if v.Latitude != nil {
		s.hasLatitude = true
		s.latitude = *v.Latitude
	}
	if v.Longitude != nil {
		s.hasLongitude = true
		s.longitude = *v.Longitude
	}
	if v.Locality != nil {
		s.hasLocality = true
		s.locality = *v.Locality
	}
	return s
}
func (s locationSnapshot) value() Location {
	v := Location{Kind: s.kind}
	if s.hasLatitude {
		x := s.latitude
		v.Latitude = &x
	}
	if s.hasLongitude {
		x := s.longitude
		v.Longitude = &x
	}
	if s.hasLocality {
		x := s.locality
		v.Locality = &x
	}
	return v
}

type postSnapshot struct {
	id, windowID, caption, visibility string
	author                            userSnapshot
	image                             any
	location                          locationSnapshot
	postedAt, deadline                time.Time
}

func (a *App) snapshotPost(p *Post) postSnapshot {
	u := a.s.Accounts[p.AuthorID]
	return postSnapshot{p.ID, p.WindowID, p.Caption, p.Visibility, a.snapshotUser(u, true), a.snapshotImage(p.ImageID, u.Secret), snapshotLocation(p.Location), p.PostedAt, a.s.Windows[p.WindowID].Deadline}
}
func (p postSnapshot) object() object {
	late := int64(p.postedAt.Sub(p.deadline) / time.Second)
	if late < 0 {
		late = 0
	}
	return object{"id": p.id, "author": p.author.object(), "postingWindowId": p.windowID, "imageUrl": p.image, "caption": p.caption, "visibility": p.visibility, "location": p.location.value(), "postedAt": p.postedAt, "onTime": !p.postedAt.After(p.deadline), "lateSeconds": late}
}

type commentSnapshot struct {
	id, body  string
	author    userSnapshot
	createdAt time.Time
}

func (a *App) snapshotComment(c *Comment) commentSnapshot {
	return commentSnapshot{c.ID, c.Body, a.snapshotUser(a.s.Accounts[c.AuthorID], true), c.CreatedAt}
}
func (c commentSnapshot) object() object {
	return object{"id": c.id, "body": c.body, "author": c.author.object(), "createdAt": c.createdAt}
}

type reactionSnapshot struct {
	id, kind  string
	reactor   userSnapshot
	image     any
	createdAt time.Time
}

func (a *App) snapshotReaction(r *Reaction) reactionSnapshot {
	u := a.s.Accounts[r.ReactorID]
	return reactionSnapshot{r.ID, r.Type, a.snapshotUser(u, true), a.snapshotImage(r.ImageID, u.Secret), r.CreatedAt}
}
func (r reactionSnapshot) object() object {
	return object{"id": r.id, "reactionType": r.kind, "reactor": r.reactor.object(), "imageUrl": r.image, "createdAt": r.createdAt}
}

type rankingSnapshot struct {
	friends map[string]bool
	graph   map[string][]string
}

func (s *State) snapshotRanking(uid string) *rankingSnapshot {
	r := &rankingSnapshot{friends: make(map[string]bool, len(s.Adj[uid])), graph: map[string][]string{}}
	for id := range s.Adj[uid] {
		r.friends[id] = true
	}
	return r
}
func (r *rankingSnapshot) capture(s *State, id string) {
	if _, ok := r.graph[id]; ok {
		return
	}
	ids := make([]string, 0, len(s.Adj[id]))
	for other := range s.Adj[id] {
		ids = append(ids, other)
	}
	r.graph[id] = ids
}
func (r *rankingSnapshot) common(id string) int {
	n := 0
	for _, other := range r.graph[id] {
		if r.friends[other] {
			n++
		}
	}
	return n
}
func (r *rankingSnapshot) score(author string, engaged []string) int {
	unique := map[string]bool{}
	for _, id := range engaged {
		if r.friends[id] {
			unique[id] = true
		}
	}
	n := 0
	for _, id := range r.graph[author] {
		n += r.common(id)
	}
	if n > 30 {
		n = 30
	}
	return 10*len(unique) + 3*r.common(author) + n
}
