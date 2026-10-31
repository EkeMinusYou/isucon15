package main

import (
	"iter"
	"sync"
)

const stateShardCount = 64

type mapShard[K comparable, V any] struct {
	mu     sync.RWMutex
	values map[K]V
}
type shardedMap[K comparable, V any] struct {
	shards [stateShardCount]mapShard[K, V]
}

func stateHash[K comparable](key K) uint64 {
	h := uint64(1469598103934665603)
	switch v := any(key).(type) {
	case string:
		for i := range v {
			h = (h ^ uint64(v[i])) * 1099511628211
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
func newShardedMapWith[K comparable, V any](values map[K]V) *shardedMap[K, V] {
	m := new(shardedMap[K, V])
	for k, v := range values {
		m.Set(k, v)
	}
	return m
}
func (m *shardedMap[K, V]) Get(key K) V {
	b := &m.shards[stateHash(key)%stateShardCount]
	b.mu.RLock()
	v := b.values[key]
	b.mu.RUnlock()
	return v
}
func (m *shardedMap[K, V]) Load(key K) (V, bool) {
	b := &m.shards[stateHash(key)%stateShardCount]
	b.mu.RLock()
	v, ok := b.values[key]
	b.mu.RUnlock()
	return v, ok
}
func (m *shardedMap[K, V]) Set(key K, value V) {
	b := &m.shards[stateHash(key)%stateShardCount]
	b.mu.Lock()
	if b.values == nil {
		b.values = make(map[K]V)
	}
	b.values[key] = value
	b.mu.Unlock()
}
func (m *shardedMap[K, V]) Delete(key K) {
	b := &m.shards[stateHash(key)%stateShardCount]
	b.mu.Lock()
	delete(b.values, key)
	b.mu.Unlock()
}
func (m *shardedMap[K, V]) Len() int {
	n := 0
	for i := range m.shards {
		b := &m.shards[i]
		b.mu.RLock()
		n += len(b.values)
		b.mu.RUnlock()
	}
	return n
}

// Callers hold logical transaction locks. Iteration must not mutate this map.
func (m *shardedMap[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for i := range m.shards {
			more := func() bool {
				b := &m.shards[i]
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
