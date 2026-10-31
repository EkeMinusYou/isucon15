package main

import (
	"maps"
	"slices"
)

type stateSignal struct{ user, region string }

func (a *App) state() *State {
	if a.view != nil {
		return a.view
	}
	return a.s
}
func (a *App) signalUser(user string) {
	if a.view != nil {
		a.signals = append(a.signals, stateSignal{user: user})
	} else {
		a.notifier.userChanged(user)
	}
}
func (a *App) signalRegion(region string) {
	if a.view != nil {
		a.signals = append(a.signals, stateSignal{region: region})
	} else {
		a.notifier.regionChanged(region)
	}
}
func (a *App) publishSignals() {
	for _, signal := range a.signals {
		if signal.user != "" {
			a.notifier.userChanged(signal.user)
		}
		if signal.region != "" {
			a.notifier.regionChanged(signal.region)
		}
	}
	a.signals = nil
}
func copyMap[K comparable, V any](value map[K]V) map[K]V { return maps.Clone(value) }
func copySlice[V any](value []V) []V                     { return slices.Clone(value) }

// A prepared update owns its changed records and derived indexes. Committed
// readers keep seeing the parent until persistence and atomic publication.
func (s *State) fork() *State {
	n := &State{
		periodsReady:      s.periodsReady,
		search:            searchIndex{grams: forkMap(s.search.grams, copyMap[string, struct{}])},
		imageLinks:        forkMap(s.imageLinks, nil),
		initialImageLinks: s.initialImageLinks,
		LivePosts:         forkMap(s.LivePosts, copySlice[string]),
		Accounts:          forkMap(s.Accounts, nil),
		Names:             forkMap(s.Names, nil),
		Admins:            forkMap(s.Admins, nil),
		Windows:           forkMap(s.Windows, nil),
		Current:           forkMap(s.Current, nil),
		UserPosts:         forkMap(s.UserPosts, copySlice[string]),
		PostPeriods:       forkMap(s.PostPeriods, copySlice[datedPostRef]),
		PostReactions:     forkMap(s.PostReactions, copySlice[string]),
		PostComments:      forkMap(s.PostComments, copySlice[string]),
		Requests:          forkMap(s.Requests, nil),
		RequestOut:        forkMap(s.RequestOut, copyMap[string, *Relation]),
		RequestIn:         forkMap(s.RequestIn, copyMap[string, *Relation]),
		Friends:           forkMap(s.Friends, nil),
		Adj:               forkMap(s.Adj, copyMap[string, bool]),
		Follows:           forkMap(s.Follows, nil),
		Following:         forkMap(s.Following, copyMap[string, bool]),
		Blocks:            forkMap(s.Blocks, nil),
		Sessions:          forkMap(s.Sessions, nil),
		UserTokens:        forkMap(s.UserTokens, copyMap[string, bool]),
		AdminSessions:     forkMap(s.AdminSessions, nil),
		Bans:              forkMap(s.Bans, nil),
		Notifications:     forkMap(s.Notifications, copySlice[Notification]),
		Posts:             s.Posts.fork(),
		Reactions:         s.Reactions.fork(),
		Comments:          s.Comments.fork(),
		Images:            s.Images.fork(),
	}
	n.Images.decorate = func(id string, im *Image) {
		key, _ := uuidKey(id)
		if link, ok := n.imageLink(key); ok {
			im.Ref, im.Kind = uuidString(link.Ref), link.Kind
		}
	}
	return n
}

func (s *State) seal() {
	s.imageLinks.seal()
	s.LivePosts.seal()
	s.Accounts.seal()
	s.Names.seal()
	s.Admins.seal()
	s.Windows.seal()
	s.Current.seal()
	s.UserPosts.seal()
	s.PostPeriods.seal()
	s.PostReactions.seal()
	s.PostComments.seal()
	s.Requests.seal()
	s.RequestOut.seal()
	s.RequestIn.seal()
	s.Friends.seal()
	s.Adj.seal()
	s.Follows.seal()
	s.Following.seal()
	s.Blocks.seal()
	s.Sessions.seal()
	s.UserTokens.seal()
	s.AdminSessions.seal()
	s.Bans.seal()
	s.Notifications.seal()
	s.Posts.live.seal()
	s.Reactions.live.seal()
	s.Comments.live.seal()
	s.Images.live.seal()
	s.search.grams.seal()
}
