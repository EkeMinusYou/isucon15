package main

import (
	"iter"
	"maps"
	"sync"
)

const stateShardCount = 64

type mapShard[K comparable, V any] struct {
	mu     sync.RWMutex
	values map[K]V
	copied map[K]bool
}
type shardedMap[K comparable, V any] struct {
	shards    [stateShardCount]*mapShard[K, V]
	owned     [stateShardCount]bool
	copyValue func(V) V
}

func stateHash[K comparable](key K) uint64 {
	h := uint64(1469598103934665603)
	switch v := any(key).(type) {
	case string:
		for i := range v {
			h = (h ^ uint64(v[i])) * 1099511628211
		}
	case uint32:
		for n := 0; n < 4; n++ {
			h = (h ^ uint64(byte(v))) * 1099511628211
			v >>= 8
		}
	case [16]byte:
		for _, b := range v {
			h = (h ^ uint64(b)) * 1099511628211
		}
	default:
		panic("unsupported state map key")
	}
	return h
}
func forkMap[K comparable, V any](parent *shardedMap[K, V], copyValue func(V) V) *shardedMap[K, V] {
	n := &shardedMap[K, V]{copyValue: copyValue}
	if parent != nil {
		n.shards = parent.shards
	}
	return n
}

func (m *shardedMap[K, V]) own(i uint64) *mapShard[K, V] {
	if !m.owned[i] {
		var values map[K]V
		if old := m.shards[i]; old != nil {
			values = maps.Clone(old.values)
		}
		if values == nil {
			values = make(map[K]V)
		}
		m.shards[i] = &mapShard[K, V]{values: values}
		m.owned[i] = true
	}
	return m.shards[i]
}

// Published shards never change. A private writer owns only changed shards
// and copies mutable leaf values at most once in that private version.
func (m *shardedMap[K, V]) seal() {
	m.copyValue = nil
	for i, shard := range m.shards {
		if m.owned[i] && shard != nil {
			shard.copied = nil
		}
	}
}
func (m *shardedMap[K, V]) Get(key K) V {
	v, _ := m.Load(key)
	return v
}
func (m *shardedMap[K, V]) Load(key K) (V, bool) {
	i := stateHash(key) % stateShardCount
	b := m.shards[i]
	if b == nil {
		var zero V
		return zero, false
	}
	if m.copyValue != nil {
		v, ok := b.values[key]
		if ok && (!m.owned[i] || !b.copied[key]) {
			b = m.own(i)
			v = m.copyValue(v)
			b.values[key] = v
			if b.copied == nil {
				b.copied = make(map[K]bool)
			}
			b.copied[key] = true
		}
		return v, ok
	}
	b.mu.RLock()
	v, ok := b.values[key]
	b.mu.RUnlock()
	return v, ok
}
func (m *shardedMap[K, V]) Set(key K, value V) {
	b := m.own(stateHash(key) % stateShardCount)
	b.mu.Lock()
	b.values[key] = value
	if m.copyValue != nil {
		if b.copied == nil {
			b.copied = make(map[K]bool)
		}
		b.copied[key] = true
	}
	b.mu.Unlock()
}
func (m *shardedMap[K, V]) Delete(key K) {
	i := stateHash(key) % stateShardCount
	if m.shards[i] == nil {
		return
	}
	b := m.own(i)
	b.mu.Lock()
	delete(b.values, key)
	b.mu.Unlock()
}
func (m *shardedMap[K, V]) Len() int {
	n := 0
	for _, b := range m.shards {
		if b != nil {
			b.mu.RLock()
			n += len(b.values)
			b.mu.RUnlock()
		}
	}
	return n
}

// Readers iterate immutable shards. Private writers copy mutable leaves before
// exposing them to mutation code.
func (m *shardedMap[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if m.copyValue != nil {
			for _, shard := range m.shards {
				if shard == nil {
					continue
				}
				keys := make([]K, 0, len(shard.values))
				for key := range shard.values {
					keys = append(keys, key)
				}
				for _, key := range keys {
					if !yield(key, m.Get(key)) {
						return
					}
				}
			}
			return
		}
		for _, b := range m.shards {
			if b == nil {
				continue
			}
			more := func() bool {
				b.mu.RLock()
				defer b.mu.RUnlock()
				for k, v := range b.values {
					if !yield(k, v) {
						return false
					}
				}
				return true
			}()
			if !more {
				return
			}
		}
	}
}
func (m *shardedMap[K, V]) Snapshot() map[K]V {
	values := make(map[K]V)
	for k, v := range m.All() {
		values[k] = v
	}
	return values
}
