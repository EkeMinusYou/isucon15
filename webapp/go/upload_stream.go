package main

import (
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type stagedImageWriter struct {
	file *os.File
	err  error
	size int64
}

func (w *stagedImageWriter) Write(b []byte) (int, error) {
	w.size += int64(len(b))
	if w.err == nil {
		n, err := w.file.Write(b)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
		w.err = err
	}
	// Keep validating the incoming stream if persistence has failed; the
	// acceptance path reports the storage error after checking request fields.
	return len(b), nil
}

func (a *App) stageImage(body io.Reader, maxDimension int) (p preparedUpload) {
	return a.stageImageNamed(body, maxDimension, id())
}

func (a *App) stageImageNamed(body io.Reader, maxDimension int, fileID string) (p preparedUpload) {
	started := time.Now()
	p.fileID = fileID
	directory := filepath.Join(a.durable, "images")
	if a.localUploads != nil {
		directory = a.localUploads.pendingDir()
	}
	f, err := os.OpenFile(filepath.Join(directory, p.fileID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		p.fileID = ""
	}
	writer := &stagedImageWriter{file: f, err: err}
	defer func() {
		if f != nil {
			if err := f.Close(); writer.err == nil {
				writer.err = err
			}
		}
		p.size, p.storageErr = writer.size, writer.err
		a.uploadStageBytes.Add(uint64(p.size))
		a.uploadStageNS.Add(uint64(time.Since(started)))
	}()
	cfg, err := jpeg.DecodeConfig(io.TeeReader(body, writer))
	if err != nil {
		p.invalidJPEG = true
		return
	}
	if cfg.Width > maxDimension || cfg.Height > maxDimension {
		return
	}
	if _, err = copyStream(writer, body); err != nil {
		p.invalidJPEG = true
		return
	}
	p.ok = true
	return
}

func (a *App) stageMultipartImage(r *http.Request, maxDimension int) preparedUpload {
	return a.stageMultipartImageNamed(r, maxDimension, id())
}

func (a *App) stageMultipartImageNamed(r *http.Request, maxDimension int, fileID string) preparedUpload {
	var p preparedUpload
	found := readMultipartSelected(r, func(part io.Reader) error {
		p = a.stageImageNamed(part, maxDimension, fileID)
		_, err := io.Copy(io.Discard, part)
		return err
	})
	if !found {
		p.ok = false
	}
	return p
}
