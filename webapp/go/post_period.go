package main

import (
	"sort"
	"strconv"
	"strings"
)

type datedPostRef struct {
	day int32
	id  string
}

func dateNumber(date string) int32 {
	n, _ := strconv.ParseInt(strings.ReplaceAll(date, "-", ""), 10, 32)
	return int32(n)
}

func (s *State) indexPostPeriod(p *Post) {
	w := s.Windows[p.WindowID]
	if w == nil {
		return
	}
	entry := datedPostRef{dateNumber(w.LocalDate), p.ID}
	entries := s.PostPeriods[p.AuthorID]
	if !s.periodsReady {
		s.PostPeriods[p.AuthorID] = append(entries, entry)
		return
	}
	i := sort.Search(len(entries), func(i int) bool { return entries[i].day > entry.day })
	entries = append(entries, datedPostRef{})
	copy(entries[i+1:], entries[i:])
	entries[i] = entry
	s.PostPeriods[p.AuthorID] = entries
}

func (s *State) finishPostPeriods() {
	for _, entries := range s.PostPeriods {
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].day < entries[j].day })
	}
	s.periodsReady = true
}

// The returned index view is used only while holding the state lock.
func (s *State) postsInPeriod(uid string, start, end int32) []datedPostRef {
	entries := s.PostPeriods[uid]
	i := sort.Search(len(entries), func(i int) bool { return entries[i].day >= start })
	j := sort.Search(len(entries), func(i int) bool { return entries[i].day >= end })
	return entries[i:j]
}
