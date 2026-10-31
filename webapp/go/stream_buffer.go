package main

import (
	"io"
	"sync"
)

var streamBuffers = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

func copyStream(dst io.Writer, src io.Reader) (int64, error) {
	b := streamBuffers.Get().(*[]byte)
	defer streamBuffers.Put(b)
	return io.CopyBuffer(dst, src, *b)
}
