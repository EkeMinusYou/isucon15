package main

// Byte trigrams are necessary conditions for UTF-8 substring matches too.
// The original normalized strings remain the authority for the final match.
type searchIndex struct {
	grams *shardedMap[uint32, map[string]struct{}]
}

func searchGrams(texts ...string) map[uint32]struct{} {
	out := map[uint32]struct{}{}
	for _, text := range texts {
		for i := 0; i+2 < len(text); i++ {
			out[uint32(text[i])<<16|uint32(text[i+1])<<8|uint32(text[i+2])] = struct{}{}
		}
	}
	return out
}

func (index *searchIndex) replace(old, current *Account) {
	if old != nil && old.searchUsername == current.searchUsername && old.searchDisplay == current.searchDisplay {
		return
	}
	fresh := searchGrams(current.searchUsername, current.searchDisplay)
	if index.grams == nil {
		index.grams = new(shardedMap[uint32, map[string]struct{}])
	}
	if old != nil {
		for gram := range searchGrams(old.searchUsername, old.searchDisplay) {
			delete(index.grams.Get(gram), current.ID)
			if len(index.grams.Get(gram)) == 0 {
				index.grams.Delete(gram)
			}
		}
	}
	for gram := range fresh {
		if index.grams.Get(gram) == nil {
			index.grams.Set(gram, map[string]struct{}{})
		}
		index.grams.Get(gram)[current.ID] = struct{}{}
	}
}

func (index *searchIndex) candidates(term string) ([]string, bool) {
	if len(term) < 3 {
		return nil, false
	}
	if index.grams == nil {
		return nil, true
	}
	var smallest map[string]struct{}
	for gram := range searchGrams(term) {
		set := index.grams.Get(gram)
		if len(set) == 0 {
			return nil, true
		}
		if smallest == nil || len(set) < len(smallest) {
			smallest = set
		}
	}
	ids := make([]string, 0, len(smallest))
	for id := range smallest {
		ids = append(ids, id)
	}
	return ids, true
}
