package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Historical rows stay in a read-only mapping. The fixed-width index contains
// no Go pointers, so allocation-heavy requests do not repeatedly trace history.
// Mutations are an overlay, persisted by the existing journal.
type recordSpan struct {
	Offset uint64
	Length uint32
}
type recordStore[T any] struct {
	live     map[string]*T
	index    map[[16]byte]recordSpan
	data     []byte
	file     *os.File
	writer   *bufio.Writer
	offset   uint64
	decorate func(string, *T)
}

func newRecordStore[T any]() *recordStore[T] {
	return &recordStore[T]{live: map[string]*T{}, index: map[[16]byte]recordSpan{}}
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
	if v := s.live[id]; v != nil {
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
	if err := json.Unmarshal(s.data[span.Offset:span.Offset+uint64(span.Length)], &v); err != nil {
		panic(fmt.Errorf("decode immutable record: %w", err))
	}
	if s.decorate != nil {
		s.decorate(id, &v)
	}
	return &v
}
func (s *recordStore[T]) set(id string, v *T) { s.live[id] = v }
func (s *recordStore[T]) start(dir string) error {
	f, err := os.CreateTemp(dir, "history-*.jsonl")
	if err != nil {
		return err
	}
	s.file = f
	s.writer = bufio.NewWriterSize(f, 1<<20)
	return nil
}
func (s *recordStore[T]) add(id string, raw []byte, v *T) error {
	key, ok := uuidKey(id)
	if !ok {
		s.set(id, v)
		return nil
	}
	if _, err := s.writer.Write(raw); err != nil {
		return err
	}
	s.index[key] = recordSpan{s.offset, uint32(len(raw))}
	s.offset += uint64(len(raw))
	return nil
}
func (s *recordStore[T]) finish() error {
	if err := s.writer.Flush(); err != nil {
		return err
	}
	if s.offset != 0 {
		data, err := unix.Mmap(int(s.file.Fd()), 0, int(s.offset), unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			return err
		}
		s.data = data
	}
	name := s.file.Name()
	err := s.file.Close()
	s.file = nil
	s.writer = nil
	if err != nil {
		return err
	}
	return os.Remove(name)
}
func (s *recordStore[T]) close() {
	if s.data != nil {
		unix.Munmap(s.data)
		s.data = nil
	}
	if s.file != nil {
		name := s.file.Name()
		s.file.Close()
		os.Remove(name)
		s.file = nil
	}
}
func (s *recordStore[T]) len() int { return len(s.index) + len(s.live) }

type imageLink struct {
	Ref  [16]byte
	Kind byte
}

func (s *State) linkImage(id, ref string, kind byte) {
	key, ok := uuidKey(id)
	target, targetOK := uuidKey(ref)
	if ok && targetOK {
		s.imageLinks[key] = imageLink{target, kind}
	}
	if im := s.Images.live[id]; im != nil {
		im.Ref = ref
		im.Kind = kind
	}
}
func (s *State) closeHistory() {
	s.Posts.close()
	s.Reactions.close()
	s.Comments.close()
	s.Images.close()
}
