package main

import (
	"iter"
	"sync"
	"sync/atomic"
)

const stateShardCount = 64

type mapShard[K comparable, V any] struct {
	mu     sync.RWMutex
	values map[K]V
}
type shardedMap[K comparable, V any] struct {
	shards  atomic.Pointer[[stateShardCount]mapShard[K, V]]
	parent  *shardedMap[K, V]
	overlay *mapOverlay[K, V]
}
type mapOverlay[K comparable, V any] struct {
	mu      sync.Mutex
	values  map[K]V
	deleted map[K]bool
	clone   func(V) V
}

func forkMap[K comparable, V any](parent *shardedMap[K, V], clone func(V) V) *shardedMap[K, V] {
	return &shardedMap[K, V]{parent: parent, overlay: &mapOverlay[K, V]{clone: clone}}
}

func (m *shardedMap[K, V]) writableShards() *[stateShardCount]mapShard[K, V] {
	if value := m.shards.Load(); value != nil {
		return value
	}
	value := new([stateShardCount]mapShard[K, V])
	if m.shards.CompareAndSwap(nil, value) {
		return value
	}
	return m.shards.Load()
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
func (m *shardedMap[K, V]) Get(key K) V {
	v, _ := m.Load(key)
	return v
}
func (m *shardedMap[K, V]) Load(key K) (V, bool) {
	if o := m.overlay; o != nil {
		o.mu.Lock()
		defer o.mu.Unlock()
		if v, ok := o.values[key]; ok {
			return v, true
		}
		if o.deleted[key] {
			var zero V
			return zero, false
		}
		v, ok := m.parent.Load(key)
		if ok && o.clone != nil {
			v = o.clone(v)
			if o.values == nil {
				o.values = map[K]V{}
			}
			o.values[key] = v
		}
		return v, ok
	}
	shards := m.shards.Load()
	if shards == nil {
		var zero V
		return zero, false
	}
	b := &shards[stateHash(key)%stateShardCount]
	b.mu.RLock()
	v, ok := b.values[key]
	b.mu.RUnlock()
	return v, ok
}
func (m *shardedMap[K, V]) Set(key K, value V) {
	if o := m.overlay; o != nil {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.values == nil {
			o.values = map[K]V{}
		}
		o.values[key] = value
		delete(o.deleted, key)
		return
	}
	b := &m.writableShards()[stateHash(key)%stateShardCount]
	b.mu.Lock()
	if b.values == nil {
		b.values = make(map[K]V)
	}
	b.values[key] = value
	b.mu.Unlock()
}
func (m *shardedMap[K, V]) Delete(key K) {
	if o := m.overlay; o != nil {
		o.mu.Lock()
		defer o.mu.Unlock()
		delete(o.values, key)
		if o.deleted == nil {
			o.deleted = map[K]bool{}
		}
		o.deleted[key] = true
		return
	}
	shards := m.shards.Load()
	if shards == nil {
		return
	}
	b := &shards[stateHash(key)%stateShardCount]
	b.mu.Lock()
	delete(b.values, key)
	b.mu.Unlock()
}
func (m *shardedMap[K, V]) Len() int {
	if m.overlay != nil {
		return len(m.Snapshot())
	}
	shards := m.shards.Load()
	if shards == nil {
		return 0
	}
	n := 0
	for i := range shards {
		b := &shards[i]
		b.mu.RLock()
		n += len(b.values)
		b.mu.RUnlock()
	}
	return n
}

// Callers hold logical transaction locks. Iteration must not mutate this map.
func (m *shardedMap[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if o := m.overlay; o != nil {
			values := m.parent.Snapshot()
			o.mu.Lock()
			for k, v := range o.values {
				values[k] = v
			}
			for k := range o.deleted {
				delete(values, k)
			}
			o.mu.Unlock()
			for key := range values {
				if value, ok := m.Load(key); ok && !yield(key, value) {
					return
				}
			}
			return
		}
		shards := m.shards.Load()
		if shards == nil {
			return
		}
		for i := range shards {
			more := func() bool {
				b := &shards[i]
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
