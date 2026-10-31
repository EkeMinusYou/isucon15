package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
)

const regionalImageRequest = "X-ISU-Regional-Image"
const regionalImagePart = "X-ISU-Image-Part"
const regionalImageOffset = "X-ISU-Image-Offset"
const regionalImageSize = "X-ISU-Image-Size"

type regionalImageBody struct {
	*io.SectionReader
	file  *os.File
	stats *regionalImageMetrics
}

type regionalImageMetrics struct {
	local    atomic.Uint64
	fallback atomic.Uint64
	bytes    atomic.Uint64
}

func (b *regionalImageBody) Read(p []byte) (int, error) {
	n, err := b.SectionReader.Read(p)
	if b.stats != nil {
		b.stats.bytes.Add(uint64(n))
	}
	return n, err
}

func (b *regionalImageBody) Close() error { return b.file.Close() }

// The owner authorizes every request. Only immutable initial image bytes are
// substituted locally; mutable validity, BANs and signatures stay authoritative.
func serveRegionalImage(resp *http.Response, root string, transport http.RoundTripper, stats *regionalImageMetrics) error {
	part := resp.Header.Get(regionalImagePart)
	defer func() {
		resp.Header.Del(regionalImagePart)
		resp.Header.Del(regionalImageOffset)
		resp.Header.Del(regionalImageSize)
	}()
	if resp.StatusCode != http.StatusOK || part == "" {
		return nil
	}
	offset, err1 := strconv.ParseInt(resp.Header.Get(regionalImageOffset), 10, 64)
	size, err2 := strconv.ParseInt(resp.Header.Get(regionalImageSize), 10, 64)
	if part != filepath.Base(part) || err1 != nil || err2 != nil || offset < 0 || size < 0 {
		return fmt.Errorf("invalid regional image metadata")
	}
	file, err := os.Open(filepath.Join(root, part))
	if err == nil {
		stat, statErr := file.Stat()
		if statErr != nil || offset > stat.Size() || size > stat.Size()-offset {
			file.Close()
			err = fmt.Errorf("regional image part is incomplete")
		}
	}
	if err != nil {
		if stats != nil {
			stats.fallback.Add(1)
		}
		// A missing local original must never turn an otherwise valid image into
		// an error. Reauthorize through the owner while requesting its full body.
		resp.Body.Close()
		req := resp.Request.Clone(resp.Request.Context())
		req.Header.Del(regionalImageRequest)
		req.Body = nil
		req.GetBody = nil
		req.ContentLength = 0
		fallback, err := transport.RoundTrip(req)
		if err != nil {
			return err
		}
		*resp = *fallback
		return nil
	}
	resp.Body.Close()
	resp.Body = &regionalImageBody{io.NewSectionReader(file, offset, size), file, stats}
	if stats != nil {
		stats.local.Add(1)
	}
	resp.ContentLength = size
	resp.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	return nil
}
