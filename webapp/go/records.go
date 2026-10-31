package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"golang.org/x/sys/unix"
)

// Historical rows stay in anonymous memory, outside the filesystem page cache
// and Go's heap limit.
// The fixed-width index requires no pointer tracing by the garbage collector.
// Mutations are an overlay, persisted by the existing journal.
const recordChunkBytes = 4 << 20

type recordSpan struct {
	Offset uint64
	Length uint32
}
type recordStore[T any] struct {
	live     *shardedMap[string, *T]
	index    map[[16]byte]recordSpan
	data     [][]byte
	decorate func(string, *T)
}

func newRecordStore[T any]() *recordStore[T] {
	return &recordStore[T]{live: new(shardedMap[string, *T]), index: map[[16]byte]recordSpan{}}
}
func uuidKey(id string) (key [16]byte, ok bool) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return key, false
	}
	var text [32]byte
	j := 0
	for i := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if id[i] >= 'A' && id[i] <= 'F' {
			return key, false
		}
		text[j] = id[i]
		j++
	}
	_, err := hex.Decode(key[:], text[:])
	return key, err == nil
}
func uuidString(k [16]byte) string {
	var out [36]byte
	hex.Encode(out[:8], k[:4])
	out[8] = '-'
	hex.Encode(out[9:13], k[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], k[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], k[8:10])
	out[23] = '-'
	hex.Encode(out[24:], k[10:])
	return string(out[:])
}
func (s *recordStore[T]) get(id string) *T {
	if v := s.live.Get(id); v != nil {
		return v
	}
	key, ok := uuidKey(id)
	if !ok {
		return nil
	}
	span, ok := s.index[key]
	if !ok {
		return nil
	}
	var v T
	chunk, offset := span.Offset/recordChunkBytes, span.Offset%recordChunkBytes
	if err := json.Unmarshal(s.data[chunk][offset:offset+uint64(span.Length)], &v); err != nil {
		panic(fmt.Errorf("decode immutable record: %w", err))
	}
	if s.decorate != nil {
		s.decorate(id, &v)
	}
	return &v
}
func (s *recordStore[T]) has(id string) bool {
	if s.live.Get(id) != nil {
		return true
	}
	key, ok := uuidKey(id)
	if !ok {
		return false
	}
	_, ok = s.index[key]
	return ok
}
func (s *recordStore[T]) set(id string, v *T) { s.live.Set(id, v) }
func (s *recordStore[T]) add(id string, raw []byte, v *T) error {
	key, ok := uuidKey(id)
	if !ok {
		s.set(id, v)
		return nil
	}
	if len(raw) > recordChunkBytes {
		return fmt.Errorf("historical record exceeds chunk size: %d", len(raw))
	}
	n := len(s.data) - 1
	if n < 0 || len(s.data[n])+len(raw) > recordChunkBytes {
		block, err := unix.Mmap(-1, 0, recordChunkBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
		if err != nil {
			return err
		}
		s.data = append(s.data, block[:0])
		n++
	}
	offset := uint64(n)*recordChunkBytes + uint64(len(s.data[n]))
	s.data[n] = append(s.data[n], raw...)
	s.index[key] = recordSpan{offset, uint32(len(raw))}
	return nil
}
func (s *recordStore[T]) close() {
	for _, chunk := range s.data {
		unix.Munmap(chunk[:cap(chunk)])
	}
	s.data = nil
}
func (s *recordStore[T]) len() int { return len(s.index) + s.live.Len() }

type imageLink struct {
	Ref  [16]byte
	Kind byte
}

func (s *State) linkImage(id, ref string, kind byte) {
	key, ok := uuidKey(id)
	target, targetOK := uuidKey(ref)
	if ok && targetOK {
		s.imageLinks.Set(key, imageLink{target, kind})
	}
	if im := s.Images.live.Get(id); im != nil {
		v := *im
		v.Ref = ref
		v.Kind = kind
		s.Images.live.Set(id, &v)
	}
}
func (s *State) closeHistory() {
	s.Posts.close()
	s.Reactions.close()
	s.Comments.close()
	s.Images.close()
}
