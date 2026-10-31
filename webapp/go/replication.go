package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	measurement "github.com/EkeMinusYou/isucon-template/tools/measurement/app"
)

const replicationStreamPath = "/api/internal/replication/stream"
const replicationAckPath = "/api/internal/replication/ack"
const replicationFailurePath = "/api/internal/replication/failure"

type replicationTicket struct {
	epoch string
	done  <-chan struct{}
}
type journalTransaction struct {
	data json.RawMessage
	done chan struct{}
}
type replicationProgress struct {
	Epoch  string
	Seq    uint64
	Region string
}
type replicationFrame struct {
	Epoch   string
	Reset   bool
	Ready   bool
	First   uint64
	Entries []json.RawMessage
}

// The journal assigns a total order. ACK waits happen after state locks are
// released; a slow link does not serialize unrelated commits behind its RTT.
type replicationHub struct {
	mu        sync.Mutex
	epoch     string
	ready     bool
	closed    bool
	entries   []journalTransaction
	peers     map[string]replicationProgress
	acked     uint64
	baseDone  chan struct{}
	baseAcked bool
	changed   chan struct{}
	epochPath string
}

func readEpoch(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "replication-epoch"))
	if errors.Is(err, os.ErrNotExist) {
		epoch := id()
		return epoch, writeEpoch(dir, epoch)
	}
	return strings.TrimSpace(string(data)), err
}
func writeEpoch(dir, epoch string) error {
	path := filepath.Join(dir, "replication-epoch")
	if err := os.WriteFile(path+".tmp", []byte(epoch+"\n"), 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
func newReplicationHub(a *App, peers []string) (*replicationHub, error) {
	epoch, err := readEpoch(a.durable)
	if err != nil {
		return nil, err
	}
	h := &replicationHub{epoch: epoch, ready: true, peers: make(map[string]replicationProgress), baseDone: make(chan struct{}), changed: make(chan struct{}), epochPath: a.durable}
	for _, peer := range peers {
		h.peers[peer] = replicationProgress{}
	}
	f, err := os.Open(a.journal.Name())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 65536), 64<<20)
	for scanner.Scan() {
		h.entries = append(h.entries, journalTransaction{append(json.RawMessage(nil), scanner.Bytes()...), make(chan struct{})})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return h, nil
}
func (h *replicationHub) notify() { close(h.changed); h.changed = make(chan struct{}) }
func (h *replicationHub) append(data []byte) replicationTicket {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := journalTransaction{append(json.RawMessage(nil), data...), make(chan struct{})}
	h.entries = append(h.entries, t)
	h.notify()
	h.advance()
	return replicationTicket{h.epoch, t.done}
}
func (h *replicationHub) advance() {
	if !h.ready {
		return
	}
	minimum := uint64(len(h.entries))
	for _, peer := range h.peers {
		if peer.Epoch != h.epoch {
			return
		}
		minimum = min(minimum, peer.Seq)
	}
	if !h.baseAcked {
		close(h.baseDone)
		h.baseAcked = true
	}
	for h.acked < minimum {
		close(h.entries[h.acked].done)
		h.acked++
	}
}
func (h *replicationHub) ack(p replicationProgress) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	old, ok := h.peers[p.Region]
	if !ok || p.Epoch != h.epoch || p.Seq > uint64(len(h.entries)) {
		return false
	}
	if old.Epoch != p.Epoch || p.Seq >= old.Seq {
		h.peers[p.Region] = p
	}
	h.advance()
	return true
}
func (h *replicationHub) beginReset() (replicationTicket, error) {
	epoch := id()
	if err := writeEpoch(h.epochPath, epoch); err != nil {
		return replicationTicket{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.baseAcked {
		close(h.baseDone)
	}
	for n := h.acked; n < uint64(len(h.entries)); n++ {
		close(h.entries[n].done)
	}
	h.epoch, h.ready, h.entries, h.acked = epoch, false, nil, 0
	h.baseDone, h.baseAcked = make(chan struct{}), false
	for peer := range h.peers {
		h.peers[peer] = replicationProgress{}
	}
	h.notify()
	return replicationTicket{epoch, h.baseDone}, nil
}
func (h *replicationHub) finishReset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ready = true
	h.advance()
	h.notify()
}
func (h *replicationHub) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		if !h.baseAcked {
			close(h.baseDone)
			h.baseAcked = true
		}
		for h.acked < uint64(len(h.entries)) {
			close(h.entries[h.acked].done)
			h.acked++
		}
		h.notify()
	}
}
func (a *App) waitReplication(ctx context.Context, ticket replicationTicket) bool {
	if ticket.done == nil {
		return true
	}
	started := time.Now()
	defer func() { a.replicationWaitNS.Add(uint64(time.Since(started))) }()
	select {
	case <-ticket.done:
		a.replication.mu.Lock()
		valid := !a.replication.closed && a.replication.epoch == ticket.epoch
		a.replication.mu.Unlock()
		return valid
	case <-ctx.Done():
		return false
	}
}

func (h *replicationHub) stream(w http.ResponseWriter, r *http.Request) {
	epoch := r.URL.Query().Get("epoch")
	cursor, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if err != nil {
		failure(w, 400, "invalid_sequence")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	encoder := json.NewEncoder(w)
	first, previousReady := true, false
	for {
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return
		}
		frame := replicationFrame{Epoch: h.epoch, Ready: h.ready}
		if epoch != h.epoch || cursor > uint64(len(h.entries)) {
			epoch, cursor, frame.Reset = h.epoch, 0, true
		}
		frame.First = cursor + 1
		end := min(uint64(len(h.entries)), cursor+128)
		for n := cursor; n < end; n++ {
			frame.Entries = append(frame.Entries, h.entries[n].data)
		}
		cursor = end
		frame.Ready = h.ready && end == uint64(len(h.entries))
		changed := h.changed
		send := first || frame.Reset || frame.Ready != previousReady || len(frame.Entries) > 0
		h.mu.Unlock()
		if send {
			if err := encoder.Encode(frame); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
			first, previousReady = false, frame.Ready
			continue
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}

func (a *App) replicationHTTP(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/internal/replication/") {
		return false
	}
	if !isInternal(r) {
		failure(w, 403, "forbidden")
		return true
	}
	if a.replication == nil {
		failure(w, 404, "not_found")
		return true
	}
	switch {
	case r.URL.Path == replicationStreamPath && r.Method == "GET":
		a.replication.stream(w, r)
	case r.URL.Path == replicationAckPath && r.Method == "POST":
		var progress replicationProgress
		if !decode(r, &progress) || !a.replication.ack(progress) {
			failure(w, 409, "invalid_replica_ack")
		} else {
			w.WriteHeader(204)
		}
	case r.URL.Path == replicationFailurePath && r.Method == "POST":
		var report struct{ IP, Region string }
		if !decode(r, &report) || !validRegion(report.Region) {
			failure(w, 400, "invalid_failure")
			return true
		}
		a = &App{appCore: a.appCore}
		r.Header.Set("X-Client-IP", report.IP)
		r.Header.Set("X-ISU-Source", report.Region)
		a.mu.RLock()
		a.globalGate.Lock()
		a.failIP(r, "signature", 3, 5*time.Second)
		ticket := a.commit()
		a.globalGate.Unlock()
		a.mu.RUnlock()
		if !a.waitReplication(r.Context(), ticket) {
			failure(w, 503, "replication_unavailable")
		} else {
			w.WriteHeader(204)
		}
	default:
		failure(w, 405, "method_not_allowed")
	}
	return true
}

type replicaReader struct {
	app           *App
	owner, region string
	progress      atomic.Pointer[replicationProgress]
	ready         atomic.Bool
	resets        atomic.Uint64
	applied       atomic.Uint64
	ackWake       chan struct{}
	cancel        context.CancelFunc
	done          chan struct{}
	ackDone       chan struct{}
	errors        atomic.Uint64
}

var replicationTransport = measurement.NewUpstreamTransport()
var replicationControl = &http.Client{Transport: replicationTransport, Timeout: 5 * time.Second}

func startReplica(a *App, owner, region string) (*replicaReader, error) {
	epoch, err := readEpoch(a.durable)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(a.journal.Name())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var seq uint64
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 65536), 64<<20)
	for scanner.Scan() {
		seq++
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	replica := &replicaReader{app: a, owner: strings.TrimRight(owner, "/"), region: region, ackWake: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{}), ackDone: make(chan struct{})}
	replica.progress.Store(&replicationProgress{epoch, seq, region})
	go func() { defer close(replica.done); replica.run(ctx) }()
	go func() { defer close(replica.ackDone); replica.ackLoop(ctx) }()
	return replica, nil
}
func (r *replicaReader) wakeAck() {
	select {
	case r.ackWake <- struct{}{}:
	default:
	}
}
func pauseReplication(ctx context.Context) bool {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
func (r *replicaReader) run(ctx context.Context) {
	client := &http.Client{Transport: replicationTransport}
	lastError := ""
	for ctx.Err() == nil {
		progress := r.progress.Load()
		address := r.owner + replicationStreamPath + "?epoch=" + url.QueryEscape(progress.Epoch) + "&after=" + strconv.FormatUint(progress.Seq, 10)
		req, _ := http.NewRequestWithContext(ctx, "GET", address, nil)
		req.Header.Set("X-ISU-Transport", transportSecret)
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == 200 {
				decoder := json.NewDecoder(resp.Body)
				for {
					var frame replicationFrame
					if err = decoder.Decode(&frame); err != nil {
						break
					}
					if err = r.apply(frame); err != nil {
						break
					}
				}
			}
			resp.Body.Close()
		}
		if ctx.Err() == nil && err != nil {
			r.errors.Add(1)
			if err.Error() != lastError {
				log.Printf("replica %s: %v", r.region, err)
				lastError = err.Error()
			}
		}
		r.ready.Store(false)
		if !pauseReplication(ctx) {
			return
		}
	}
}
func (r *replicaReader) apply(frame replicationFrame) error {
	previous := r.progress.Load()
	if frame.Reset {
		r.ready.Store(false)
		r.app.mu.Lock()
		err := r.app.resetLocal()
		if err == nil && r.app.localUploads != nil {
			err = r.app.localUploads.reset()
		}
		if err == nil {
			err = writeEpoch(r.app.durable, frame.Epoch)
		}
		r.app.mu.Unlock()
		if err != nil {
			return err
		}
		r.resets.Add(1)
		previous = &replicationProgress{Epoch: frame.Epoch, Region: r.region}
		r.progress.Store(previous)
	}
	if frame.Epoch != previous.Epoch {
		return fmt.Errorf("replication epoch mismatch")
	}
	if len(frame.Entries) > 0 {
		if frame.First != previous.Seq+1 {
			return fmt.Errorf("replication sequence gap")
		}
		r.app.mu.Lock()
		defer r.app.mu.Unlock()
		for _, data := range frame.Entries {
			var changes []change
			if err := json.Unmarshal(data, &changes); err != nil {
				return err
			}
			line := append(append([]byte(nil), data...), '\n')
			if _, err := r.app.journal.Write(line); err != nil {
				return err
			}
			for _, c := range changes {
				if err := r.app.s.apply(c.Kind, c.Data); err != nil {
					return err
				}
			}
			r.notify(changes)
		}
		previous = &replicationProgress{frame.Epoch, previous.Seq + uint64(len(frame.Entries)), r.region}
		r.progress.Store(previous)
		r.applied.Add(uint64(len(frame.Entries)))
	}
	r.ready.Store(frame.Ready)
	r.wakeAck()
	return nil
}
func (r *replicaReader) notify(changes []change) {
	for _, c := range changes {
		switch c.Kind {
		case "account":
			var account Account
			if json.Unmarshal(c.Data, &account) == nil {
				r.app.notifier.userChanged(account.ID)
			}
		case "window":
			var window Window
			if json.Unmarshal(c.Data, &window) == nil {
				r.app.notifier.regionChanged(window.Region)
			}
		}
	}
}
func (r *replicaReader) ackLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.ackWake:
		}
		for ctx.Err() == nil {
			progress := r.progress.Load()
			data, _ := json.Marshal(progress)
			req, _ := http.NewRequestWithContext(ctx, "POST", r.owner+replicationAckPath, strings.NewReader(string(data)))
			req.Header.Set("X-ISU-Transport", transportSecret)
			resp, err := replicationControl.Do(req)
			ok := err == nil && resp.StatusCode == 204
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			if ok {
				break
			}
			if !pauseReplication(ctx) {
				return
			}
		}
	}
}
func (a *App) stopReplication() {
	if a.replica != nil {
		a.replica.cancel()
		<-a.replica.done
		<-a.replica.ackDone
	}
	if a.replication != nil {
		a.replication.stop()
	}
}

func (a *App) reportReplicaFailure(r *http.Request) bool {
	body, _ := json.Marshal(struct{ IP, Region string }{ipOf(r), a.replica.region})
	req, _ := http.NewRequestWithContext(r.Context(), "POST", a.replica.owner+replicationFailurePath, strings.NewReader(string(body)))
	req.Header.Set("X-ISU-Transport", transportSecret)
	resp, err := replicationControl.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == 204
}

func (a *App) ownedOriginalHTTP(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, regionalFilePrefix) {
		return false
	}
	if !isInternal(r) {
		failure(w, 403, "forbidden")
		return true
	}
	if r.Method != "GET" {
		failure(w, 405, "method_not_allowed")
		return true
	}
	imageID := strings.TrimPrefix(r.URL.Path, regionalFilePrefix)
	if _, ok := uuidKey(imageID); !ok {
		failure(w, 404, "image_unavailable")
		return true
	}
	unlock := a.lockReadRequest(r)
	im := a.s.Images.get(imageID)
	var metadata Image
	if im != nil {
		metadata = *im
	}
	unlock()
	if im == nil {
		failure(w, 404, "image_unavailable")
		return true
	}
	path := filepath.Join(a.durable, "images", imageID)
	if metadata.Part != "" {
		path = filepath.Join(a.initial, "images", metadata.Part)
	}
	f, err := os.Open(path)
	if err != nil {
		failure(w, 404, "image_unavailable")
		return true
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.FormatInt(metadata.Size, 10))
	copyStream(w, io.NewSectionReader(f, metadata.Offset, metadata.Size))
	return true
}
