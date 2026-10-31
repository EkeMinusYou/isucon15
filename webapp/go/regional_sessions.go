package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const sessionInternalPrefix = "/api/internal/auth/"
const authenticatedUserHeader = "X-ISU-Authenticated-User"
const ingressFailureHeader = "X-ISU-Ingress-Failure"

type authenticatedUserKey struct{}
type sessionRecord struct{ User, Token string }
type sessionRequest struct{ Username, SecretKey, Token, IP string }
type sessionAnswer struct {
	ID    string `json:"id,omitempty"`
	Token string `json:"token,omitempty"`
	User  object `json:"user,omitempty"`
}

// Only the authentication owner writes these sessions. A journal entry replaces
// a user's previous token; session changes never enter public-state replication.
type regionalSessions struct {
	mu        sync.RWMutex
	file      *os.File
	users     map[string]string
	tokens    map[string]string
	failureMu sync.Mutex
	failures  map[string][]time.Time
	updates   atomic.Uint64
	checks    atomic.Uint64
	remote    atomic.Uint64
	failed    atomic.Uint64
	bans      atomic.Uint64
}

func openRegionalSessions(dir string) (*regionalSessions, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "sessions.jsonl"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	s := &regionalSessions{file: f, users: map[string]string{}, tokens: map[string]string{}, failures: map[string][]time.Time{}}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var record sessionRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			f.Close()
			return nil, err
		}
		s.apply(record)
	}
	if err := scanner.Err(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

func (s *regionalSessions) apply(record sessionRecord) {
	delete(s.tokens, s.users[record.User])
	delete(s.users, record.User)
	if record.Token != "" {
		s.users[record.User], s.tokens[record.Token] = record.Token, record.User
	}
}
func (s *regionalSessions) append(record sessionRecord) error {
	data, _ := json.Marshal(record)
	data = append(data, '\n')
	if n, err := s.file.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	s.apply(record)
	s.updates.Add(1)
	return nil
}
func (s *regionalSessions) lookup(token string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.checks.Add(1)
	return s.tokens[token]
}
func (s *regionalSessions) replace(user, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(sessionRecord{user, token})
}
func (s *regionalSessions) logout(token string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.tokens[token]
	if user == "" {
		return false, nil
	}
	return true, s.append(sessionRecord{User: user})
}
func (s *regionalSessions) reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.file.Truncate(0); err != nil {
		return err
	}
	s.users, s.tokens = map[string]string{}, map[string]string{}
	s.failureMu.Lock()
	s.failures = map[string][]time.Time{}
	s.failureMu.Unlock()
	return nil
}

func authenticationRegion(user *Account) string {
	if user.AuthRegion != "" {
		return user.AuthRegion
	}
	return user.Region
}

func sessionToken(region string) string {
	token := []byte(id())
	value := strings.IndexByte("0123456789abcdef", token[0]) & 3
	if region == "europe" {
		value += 4
	} else if region == "america" {
		value += 8
	}
	token[0] = "0123456789abcdef"[value]
	return string(token)
}
func tokenRegion(token string) string {
	if _, ok := uuidKey(token); !ok {
		return ""
	}
	switch strings.IndexByte("0123456789abcdef", token[0]) / 4 {
	case 0:
		return "asia"
	case 1:
		return "europe"
	case 2:
		return "america"
	}
	return ""
}

func (a *App) enableRegionalSessions(region string) error {
	a.region = region
	if os.Getenv("REGIONAL_SESSIONS") != "1" {
		return nil
	}
	var err error
	a.sessions, err = openRegionalSessions(envOr("SESSION_DIR", "/home/isucon/isureal-sessions"))
	return err
}
func (a *App) requestActor(r *http.Request) string {
	if user, ok := r.Context().Value(authenticatedUserKey{}).(string); ok {
		return user
	}
	if a.sessions != nil {
		return ""
	}
	return a.s.Sessions.Get(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}
func needsSession(r *http.Request) bool {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/api/admin/") {
		return false
	}
	if r.Method == "POST" && (path == "/api/initialize" || path == "/api/auth/register" || path == "/api/auth/login") {
		return false
	}
	return r.Method != "GET" || !strings.HasPrefix(path, "/api/images/")
}

func (a *App) sessionRPC(ctx context.Context, region, action string, input sessionRequest) (sessionAnswer, int) {
	var answer sessionAnswer
	address := regionalPeer(region)
	if address == "" {
		return answer, 503
	}
	data, _ := json.Marshal(input)
	req, _ := http.NewRequestWithContext(ctx, "POST", address+sessionInternalPrefix+action, bytes.NewReader(data))
	req.Header.Set("X-ISU-Transport", transportSecret)
	req.Header.Set("X-ISU-Source", a.region)
	req.Header.Set("X-Client-IP", input.IP)
	a.sessions.remote.Add(1)
	resp, err := replicationControl.Do(req)
	if err != nil {
		return answer, 503
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer); err != nil {
			return answer, 503
		}
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	return answer, resp.StatusCode
}

func (a *App) localSession(action string, input sessionRequest) (sessionAnswer, int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var answer sessionAnswer
	if action == "login" {
		user := a.s.Accounts.Get(a.s.Names.Get(input.Username))
		if !usernameRE.MatchString(input.Username) || user == nil || user.Status != "active" || !hmac.Equal([]byte(user.Secret), []byte(secretHash(input.SecretKey))) {
			return answer, 401
		}
		if authenticationRegion(user) != a.region {
			return answer, 503
		}
		token := sessionToken(a.region)
		if err := a.sessions.replace(user.ID, token); err != nil {
			return answer, 503
		}
		return sessionAnswer{Token: token, User: sessionUser(user)}, 200
	}
	userID := a.sessions.lookup(input.Token)
	user := a.s.Accounts.Get(userID)
	if userID == "" || user == nil || user.Status != "active" {
		return answer, 401
	}
	if action == "logout" {
		ok, err := a.sessions.logout(input.Token)
		if err != nil {
			return answer, 503
		}
		if !ok {
			return answer, 401
		}
		return sessionAnswer{ID: userID}, 200
	}
	return sessionAnswer{ID: userID}, 200
}

func (a *App) resolveSession(r *http.Request, action string, input sessionRequest) (sessionAnswer, int) {
	region := tokenRegion(input.Token)
	if action == "login" {
		a.mu.RLock()
		user := a.s.Accounts.Get(a.s.Names.Get(input.Username))
		if user != nil {
			region = authenticationRegion(user)
		}
		a.mu.RUnlock()
	}
	if !validRegion(region) {
		return sessionAnswer{}, 401
	}
	if region == a.region {
		return a.localSession(action, input)
	}
	input.IP = ipOf(r)
	return a.sessionRPC(r.Context(), region, action, input)
}

// Called only at the original public ingress. Failure counts are independent
// of the user and authentication owner; only an actual BAN crosses regions.
func (a *App) recordIngressFailure(r *http.Request, kind string) bool {
	limit, period := 0, time.Duration(0)
	switch kind {
	case "login":
		limit, period = 10, 10*time.Second
	case "jpeg":
		limit, period = 2, 5*time.Second
	case "signature":
		limit, period = 3, 5*time.Second
	default:
		return false
	}
	s := a.sessions
	a.mu.RLock()
	s.failureMu.Lock()
	now := time.Now()
	key := kind + "/" + ipOf(r)
	times := s.failures[key][:0]
	for _, t := range s.failures[key] {
		if now.Sub(t) < period {
			times = append(times, t)
		}
	}
	times = append(times, now)
	reached := len(times) >= limit
	if reached {
		times = nil
	}
	s.failures[key] = times
	s.failed.Add(1)
	s.failureMu.Unlock()
	a.mu.RUnlock()
	if !reached {
		return true
	}
	s.bans.Add(1)
	if a.replication != nil {
		return a.establishSharedBan(r, ipOf(r))
	}
	_, status := a.sessionRPC(r.Context(), "asia", "ban", sessionRequest{IP: ipOf(r)})
	return status == 204
}
func (a *App) establishSharedBan(r *http.Request, ip string) bool {
	request := &App{appCore: a.appCore}
	a.mu.RLock()
	a.globalGate.Lock()
	until := time.Now().Add(2 * time.Minute)
	if a.s.Bans.Get(ip).Before(until) {
		request.put("ban", banChange{ip, until})
	}
	ticket := request.commit()
	a.globalGate.Unlock()
	a.mu.RUnlock()
	return a.waitReplication(r.Context(), ticket)
}

func (a *App) sessionInternalHTTP(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, sessionInternalPrefix) {
		return false
	}
	if !isInternal(r) {
		failure(w, 403, "forbidden")
		return true
	}
	if a.sessions == nil || r.Method != "POST" || (a.replica != nil && !a.replica.ready.Load()) {
		failure(w, 503, "authentication_unavailable")
		return true
	}
	var input sessionRequest
	if !decode(r, &input) {
		failure(w, 400, "invalid_request")
		return true
	}
	action := strings.TrimPrefix(r.URL.Path, sessionInternalPrefix)
	if action == "ban" {
		if a.replication == nil || !a.establishSharedBan(r, input.IP) {
			failure(w, 503, "replication_unavailable")
		} else {
			w.WriteHeader(204)
		}
		return true
	}
	if action != "login" && action != "lookup" && action != "logout" {
		failure(w, 404, "not_found")
		return true
	}
	answer, status := a.localSession(action, input)
	output(w, status, answer)
	return true
}

// Public requests authenticate once. The writer trusts only the identity on
// its authenticated internal transport, then checks live business state itself.
func (a *App) authenticateIngress(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	r.Header.Del(authenticatedUserHeader)
	r.Header.Del(ingressFailureHeader)
	if a.sessions == nil {
		return r, false
	}
	if a.replica != nil && !a.replica.ready.Load() {
		failure(w, 503, "replica_starting")
		return r, true
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		a.mu.RLock()
		banned := time.Now().Before(a.s.Bans.Get(ipOf(r)))
		a.mu.RUnlock()
		if banned {
			failure(w, 403, "ip_banned")
			return r, true
		}
	}
	login := r.URL.Path == "/api/auth/login" && r.Method == "POST"
	logout := r.URL.Path == "/api/auth/logout" && r.Method == "POST"
	if !login && !logout && !needsSession(r) {
		return r, false
	}
	action := "lookup"
	input := sessionRequest{}
	if login || logout {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if err != nil {
			failure(w, 413, "body_too_large")
			return r, true
		}
		if login {
			action = "login"
			json.NewDecoder(bytes.NewReader(data)).Decode(&input)
		}
	}
	if !login {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			input.Token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if logout {
			action = "logout"
		}
	}
	answer, status := a.resolveSession(r, action, input)
	if status == 401 {
		code := "invalid_session"
		if login {
			code = "invalid_credentials"
			if !a.recordIngressFailure(r, "login") {
				failure(w, 503, "replication_unavailable")
				return r, true
			}
		}
		failure(w, 401, code)
		return r, true
	}
	if status != 200 {
		failure(w, 503, "authentication_unavailable")
		return r, true
	}
	if login {
		if user, ok := answer.User["id"].(string); ok {
			setMeasurementIdentity(r, user)
		}
		output(w, 200, object{"token": answer.Token, "user": answer.User})
		return r, true
	}
	if logout {
		setMeasurementIdentity(r, answer.ID)
		w.WriteHeader(204)
		return r, true
	}
	setMeasurementIdentity(r, answer.ID)
	return r.WithContext(context.WithValue(r.Context(), authenticatedUserKey{}, answer.ID)), false
}

func (a *App) finishIngressFailure(r *http.Request, response *http.Response) error {
	kind := response.Header.Get(ingressFailureHeader)
	response.Header.Del(ingressFailureHeader)
	if kind != "" && !a.recordIngressFailure(r, kind) {
		return fmt.Errorf("BAN replication failed")
	}
	return nil
}
