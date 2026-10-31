package main

import (
	"maps"
	"slices"
)

func copyMap[K comparable, V any](value map[K]V) map[K]V { return maps.Clone(value) }
func copySlice[V any](value []V) []V                     { return slices.Clone(value) }

// A prepared update owns its changed records and derived indexes. Committed
// readers keep seeing the parent until the regional master has persisted it.
func (s *State) fork() *State {
	n := &State{
		periodsReady:  s.periodsReady,
		imageLinks:    forkMap(s.imageLinks, nil),
		LivePosts:     forkMap(s.LivePosts, copySlice[string]),
		Accounts:      forkMap(s.Accounts, nil),
		Names:         forkMap(s.Names, nil),
		Admins:        forkMap(s.Admins, nil),
		Windows:       forkMap(s.Windows, nil),
		Current:       forkMap(s.Current, nil),
		UserPosts:     forkMap(s.UserPosts, copySlice[string]),
		PostPeriods:   forkMap(s.PostPeriods, copySlice[datedPostRef]),
		PostReactions: forkMap(s.PostReactions, copySlice[string]),
		PostComments:  forkMap(s.PostComments, copySlice[string]),
		Requests:      forkMap(s.Requests, nil),
		RequestOut:    forkMap(s.RequestOut, copyMap[string, *Relation]),
		RequestIn:     forkMap(s.RequestIn, copyMap[string, *Relation]),
		Friends:       forkMap(s.Friends, nil),
		Adj:           forkMap(s.Adj, copyMap[string, bool]),
		Follows:       forkMap(s.Follows, nil),
		Blocks:        forkMap(s.Blocks, nil),
		Sessions:      forkMap(s.Sessions, nil),
		UserTokens:    forkMap(s.UserTokens, copyMap[string, bool]),
		AdminSessions: forkMap(s.AdminSessions, nil),
		Bans:          forkMap(s.Bans, nil),
		Notifications: forkMap(s.Notifications, copySlice[Notification]),
		Posts:         s.Posts.fork(),
		Reactions:     s.Reactions.fork(),
		Comments:      s.Comments.fork(),
		Images:        s.Images.fork(),
	}
	n.Images.decorate = func(id string, im *Image) {
		key, _ := uuidKey(id)
		if link, ok := n.imageLinks.Load(key); ok {
			im.Ref, im.Kind = uuidString(link.Ref), link.Kind
		}
	}
	return n
}
