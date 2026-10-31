package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const imagePermitLifetime = 4500 * time.Millisecond
const imagePermitHeader = "X-ISU-Image-Permits"
const imageRevokePath = "/api/internal/region/revoke-images"

type imagePermit struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Part      string `json:"part"`
	Offset    int64  `json:"offset"`
	Size      int64  `json:"size"`
	Exp       int64  `json:"exp"`
	MediaType string `json:"mediaType"`
	Uploaded  bool   `json:"uploaded,omitempty"`
	Post      bool   `json:"post,omitempty"`
}

type heldImagePermit struct {
	imagePermit
	region   string
	deadline time.Time
	deps     []string
}

// This registry contains only short-lived grants, not a replica of user or
// image state. A grant is installed while the corresponding state is locked.
type ownerImagePermits struct {
	mu          sync.Mutex
	prefix      string
	next        uint64
	byID        map[string]*heldImagePermit
	byDep       map[string]map[string]*heldImagePermit
	expires     []*heldImagePermit
	head        int
	issued      atomic.Uint64
	revoked     atomic.Uint64
	rpcNS       atomic.Uint64
	failures    atomic.Uint64
	notifySlots chan struct{}
}

func newOwnerImagePermits() *ownerImagePermits {
	return &ownerImagePermits{prefix: id(), byID: map[string]*heldImagePermit{}, byDep: map[string]map[string]*heldImagePermit{}, notifySlots: make(chan struct{}, 32)}
}

func (p *ownerImagePermits) remove(grant *heldImagePermit) {
	delete(p.byID, grant.ID)
	for _, dep := range grant.deps {
		delete(p.byDep[dep], grant.ID)
		if len(p.byDep[dep]) == 0 {
			delete(p.byDep, dep)
		}
	}
}

func (p *ownerImagePermits) prune(now time.Time) {
	for p.head < len(p.expires) && !now.Before(p.expires[p.head].deadline) {
		grant := p.expires[p.head]
		if p.byID[grant.ID] == grant {
			p.remove(grant)
		}
		p.expires[p.head] = nil
		p.head++
	}
	if p.head > 1024 && p.head*2 > len(p.expires) {
		p.expires = append([]*heldImagePermit(nil), p.expires[p.head:]...)
		p.head = 0
	}
}

func (p *ownerImagePermits) grant(region string, wire imagePermit, deps []string) (imagePermit, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.prune(now)
	if len(p.byID) >= 65536 {
		return imagePermit{}, false
	}
	p.next++
	wire.ID = p.prefix + "-" + strconv.FormatUint(p.next, 36)
	grant := &heldImagePermit{wire, region, now.Add(imagePermitLifetime), deps}
	// Keep every still-live generation until expiry or revocation. A newer
	// response may be delayed while an older grant remains installed remotely.
	p.byID[wire.ID] = grant
	for _, dep := range deps {
		if p.byDep[dep] == nil {
			p.byDep[dep] = map[string]*heldImagePermit{}
		}
		p.byDep[dep][wire.ID] = grant
	}
	p.expires = append(p.expires, grant)
	p.issued.Add(1)
	return wire, true
}

func (p *ownerImagePermits) revoke(dep string) []*heldImagePermit {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(time.Now())
	var grants []*heldImagePermit
	if dep == "" {
		for _, grant := range p.byID {
			grants = append(grants, grant)
		}
	} else {
		for _, grant := range p.byDep[dep] {
			grants = append(grants, grant)
		}
	}
	for _, grant := range grants {
		p.remove(grant)
	}
	p.revoked.Add(uint64(len(grants)))
	return grants
}

type permitResponseKey struct{}
type permitResponse struct {
	request *http.Request
	writer  http.ResponseWriter
}

func issueImagePermits(ctx context.Context, refs []imageReference) {
	response, _ := ctx.Value(permitResponseKey{}).(*permitResponse)
	if response == nil || len(refs) == 0 {
		return
	}
	var grants []imagePermit
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref.id] || len(grants) == 128 {
			continue
		}
		seen[ref.id] = true
		address := ref.address()
		r := response.request.Clone(ctx)
		r.Method = "GET"
		r.URL, _ = url.Parse(address)
		unlock := ref.app.lockReadRequest(r)
		grant, ok := ref.app.issueImagePermit(r, address, ref.id, ref.secret, ref.exp)
		unlock()
		if ok {
			grants = append(grants, grant)
		}
	}
	if len(grants) != 0 {
		b, _ := json.Marshal(grants)
		response.writer.Header().Add(imagePermitHeader, base64.RawURLEncoding.EncodeToString(b))
	}
}

func (a *App) issueImagePermit(r *http.Request, address, imageID, secret string, exp int64) (imagePermit, bool) {
	if a.imagePermits == nil || !isInternal(r) || exp <= time.Now().Unix() || time.Now().Before(a.s.Bans.Get(ipOf(r))) {
		return imagePermit{}, false
	}
	im := a.s.Images.get(imageID)
	if !a.validImage(im) || a.s.Accounts.Get(im.OwnerID).Secret != secret {
		return imagePermit{}, false
	}
	region := requestRegion(r)
	if im.Part == "" && im.StorageRegion != region {
		return imagePermit{}, false
	}
	wire := imagePermit{URL: address, Part: im.Part, Offset: im.Offset, Size: im.Size, Exp: exp, MediaType: im.MediaType, Post: im.Kind == 'p'}
	if wire.Part == "" {
		wire.Part, wire.Uploaded = im.ID, true
	}
	deps := []string{"image:" + imageID, "account:" + im.OwnerID, "ip:" + ipOf(r)}
	if im.Kind == 'p' {
		deps = append(deps, "post:"+im.Ref)
	} else if im.Kind == 'r' {
		reaction := a.s.Reactions.get(im.Ref)
		post := a.s.Posts.get(reaction.PostID)
		deps = append(deps, "reaction:"+im.Ref, "post:"+post.ID, "account:"+post.AuthorID)
	}
	return a.imagePermits.grant(region, wire, deps)
}

func (a *App) retireImagePermits(kind string, value any) {
	if a.imagePermits == nil {
		return
	}
	var dep string
	switch kind {
	case "account":
		var next *Account
		switch v := value.(type) {
		case Account:
			next = &v
		case *Account:
			next = v
		}
		if next != nil {
			old := a.s.Accounts.Get(next.ID)
			profile := func(u *Account) string {
				if u.ProfileImageID == nil {
					return ""
				}
				return *u.ProfileImageID
			}
			if old != nil && (old.Status != next.Status || old.Secret != next.Secret || profile(old) != profile(next)) {
				dep = "account:" + next.ID
			}
		}
	case "post":
		switch v := value.(type) {
		case Post:
			if v.DeletedAt != nil {
				dep = "post:" + v.ID
			}
		case *Post:
			if v.DeletedAt != nil {
				dep = "post:" + v.ID
			}
		}
	case "reaction":
		switch v := value.(type) {
		case Reaction:
			if v.DeletedAt != nil || v.ReplacedAt != nil {
				dep = "reaction:" + v.ID
			}
		case *Reaction:
			if v.DeletedAt != nil || v.ReplacedAt != nil {
				dep = "reaction:" + v.ID
			}
		}
	case "ban":
		if v, ok := value.(banChange); ok {
			dep = "ip:" + v.IP
		}
	}
	if dep != "" {
		a.revocations = append(a.revocations, a.imagePermits.revoke(dep)...)
		if kind == "ban" {
			a.strictImageRevocations = true
		}
	}
}

func (a *App) sendImageRevocations() {
	if len(a.revocations) == 0 {
		return
	}
	manager := a.imagePermits
	secret := transportSecret
	strict := a.strictImageRevocations
	a.strictImageRevocations = false
	byRegion := map[string][]*heldImagePermit{}
	for _, grant := range a.revocations {
		byRegion[grant.region] = append(byRegion[grant.region], grant)
	}
	a.revocations = nil
	var completed sync.WaitGroup
	for region, grants := range byRegion {
		address := regionalPeer(region) + imageRevokePath
		if !strict {
			select {
			case manager.notifySlots <- struct{}{}:
			default:
				// Expiry still enforces the five-second invalidation bound if an
				// overloaded or disconnected region cannot receive a notice.
				manager.failures.Add(1)
				continue
			}
		}
		completed.Add(1)
		go func(region string, grants []*heldImagePermit) {
			defer completed.Done()
			started := time.Now()
			defer func() {
				if !strict {
					<-manager.notifySlots
				}
				manager.rpcNS.Add(uint64(time.Since(started)))
			}()
			deadline := grants[0].deadline
			ids := make([]string, len(grants))
			for i, grant := range grants {
				ids[i] = grant.ID
				if deadline.Before(grant.deadline) {
					deadline = grant.deadline
				}
			}
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			b, _ := json.Marshal(ids)
			r, err := http.NewRequestWithContext(ctx, "POST", address, bytes.NewReader(b))
			if err == nil {
				r.Header.Set("X-ISU-Transport", secret)
				resp, requestErr := regionalClient.Do(r)
				if requestErr == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode == http.StatusNoContent {
						return
					}
				}
			}
			manager.failures.Add(1)
			if strict {
				if remaining := time.Until(deadline); remaining > 0 {
					time.Sleep(remaining)
				}
			}
		}(region, grants)
	}
	// BAN is not part of the relaxed image-deletion contract. Its triggering
	// response retains the original invalidation ordering, outside state locks.
	if strict {
		completed.Wait()
	}
}
