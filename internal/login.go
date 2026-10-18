package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// loginTTL bounds a login session. The portal flow needs a human to sign in, so
// it matches the host's own OAuth session TTL (10 minutes).
const loginTTL = 10 * time.Minute

// loginPollInterval throttles ticket polling; the reference CLI polls every 2s
// and the portal can take a few seconds to publish the ticket. It stays below
// the host's own poll cadence so a legitimate tick is never skipped.
var loginPollInterval = 1500 * time.Millisecond

const (
	loginCallbackSuccessHTML = `<html><head><meta charset="utf-8"><title>Login successful</title></head><body><h3>登录成功，可关闭此页面。</h3></body></html>`
	loginCallbackFailedHTML  = `<html><head><meta charset="utf-8"><title>Login failed</title></head><body><h3>登录失败：缺少 code</h3></body></html>`
)

// loginSession is one in-flight login. It carries everything the exchange needs,
// because the CPA host only hands back an opaque state string.
type loginSession struct {
	mu       sync.Mutex
	id       string // opaque session id returned as the auth.login.start state
	ticket   string
	secret   string // portal-issued secret overrides our generated one once seen
	verifier string
	key      upstream.DPoPPrivateJWK
	cfg      upstream.LoginConfig
	// callbackHost is the host the browser uses to reach the loopback listener.
	// It stays 127.0.0.1 outside containers and follows CODEARTS_CALLBACK_HOST
	// when the plugin runs where that loopback is not the browser's.
	callbackHost string
	port         int // redirect_uri port (loopback listener)
	// listener is the loopback callback server. The CPA host exposes no generic
	// provider callback route, so the plugin serves its own; the public base URL
	// (when the host provides one) is only used for pasted callbacks.
	listener net.Listener
	server   *http.Server
	expires  time.Time
	data     *pluginAuthData
	busy     bool
	lastPoll time.Time
	codes    chan string
}

var loginSessions = struct {
	sync.Mutex
	byID map[string]*loginSession
}{byID: make(map[string]*loginSession)}

var exchangeLoginCode = func(ctx context.Context, s *loginSession, code string) (*upstream.TokenResponse, error) {
	// redirect_uri must repeat exactly what /authorize received, host included.
	return newUpstreamClient(35*time.Second).ExchangeCodeAt(ctx, s.cfg, code, s.verifier, s.callbackHost, s.port, s.key)
}
var pollLoginTicket = func(ctx context.Context, s *loginSession) (*upstream.TokenResponse, error) {
	return newUpstreamClient(30*time.Second).PollTicket(ctx, s.cfg, s.ticket, s.secret)
}
var saveLoginAuth = func(v any) (json.RawMessage, error) { return hostAuthSave(v) }

// validLoginState mirrors the host's ValidateOAuthState: the state becomes part
// of a callback file name and is length-capped.
func validLoginState(state string) error {
	if state == "" {
		return errors.New("empty state")
	}
	if len(state) > 128 {
		return errors.New("state too long")
	}
	if strings.Contains(state, "/") || strings.Contains(state, "\\") || strings.Contains(state, "..") {
		return errors.New("state contains path characters")
	}
	for _, r := range state {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-' || r == '_' || r == '.':
		default:
			return errors.New("state contains invalid characters")
		}
	}
	return nil
}

func stopLogin(s *loginSession) {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.server != nil {
		_ = s.server.Close()
	}
}

func pruneLogins() {
	now := time.Now()
	for key, s := range loginSessions.byID {
		if now.After(s.expires) {
			delete(loginSessions.byID, key)
			stopLogin(s)
		}
	}
}

func registerLogin(s *loginSession) error {
	loginSessions.Lock()
	defer loginSessions.Unlock()
	pruneLogins()
	if len(loginSessions.byID) >= 16 {
		return errors.New("too many pending logins")
	}
	loginSessions.byID[s.id] = s
	return nil
}

func lookupLogin(id string) *loginSession {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return nil
	}
	loginSessions.Lock()
	defer loginSessions.Unlock()
	pruneLogins()
	return loginSessions.byID[id]
}

func dropLogin(id string) {
	loginSessions.Lock()
	s := loginSessions.byID[id]
	delete(loginSessions.byID, id)
	loginSessions.Unlock()
	stopLogin(s)
}

// newLoginSession builds a pending login with its own ticket, PKCE pair and DPoP
// key. Nothing is registered or listening yet.
func newLoginSession(baseURL string) (*loginSession, error) {
	cfg := upstream.DefaultLoginConfig()
	ticket, err := upstream.RandomHex(16)
	if err != nil {
		return nil, err
	}
	secret, err := upstream.RandomHex(16)
	if err != nil {
		return nil, err
	}
	id, err := upstream.RandomHex(16)
	if err != nil {
		return nil, err
	}
	verifier, _, err := upstream.PKCE()
	if err != nil {
		return nil, err
	}
	key, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		return nil, err
	}
	return &loginSession{
		id: strings.ToLower(id), ticket: ticket, secret: secret, verifier: verifier,
		key: key, cfg: cfg, callbackHost: callbackHostFor(), expires: time.Now().Add(loginTTL), codes: make(chan string, 1),
	}, nil
}

// listenLoopback binds the callback listener the portal redirects to. The
// redirect_uri port must be the listening port, otherwise the browser callback
// (the first of the portal's two callbacks) never reaches us.
//
// In a container the browser never shares this process's loopback namespace, so
// binding 127.0.0.1 would make the redirect unreachable. We bind the wildcard
// address instead; the listener still only answers the plugin's own callback
// path, and a container without a published port simply falls back to the
// ticket channel.
func (s *loginSession) listenLoopback() error {
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		if ln, err = net.Listen("tcp4", "127.0.0.1:0"); err != nil {
			return err
		}
	}
	s.listener = ln
	s.port = ln.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc(s.cfg.RedirectPath, s.serveCallback)
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.server.Serve(ln) }()
	go func() {
		timer := time.NewTimer(loginTTL)
		defer timer.Stop()
		<-timer.C
		dropLogin(s.id)
	}()
	return nil
}

// serveCallback mirrors the reference CLI (cmd/login):
//   - the portal calls back twice: first with secret+redirect (307 back to the
//     portal), then with the authorization code;
//   - a code may also arrive in a POST body;
//   - failing the first callback would break the chain, so secret/redirect is
//     always honoured before anything else is validated.
func (s *loginSession) serveCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if secret := query.Get("secret"); secret != "" {
		s.mu.Lock()
		s.secret = secret
		s.mu.Unlock()
	}
	code := query.Get("code")
	if code == "" {
		if redirect := query.Get("redirect"); redirect != "" {
			if next, err := portalRedirect(s, redirect); err == nil {
				http.Redirect(w, r, next, http.StatusTemporaryRedirect)
				return
			}
		}
		// Some portal variants post the code as a form body instead of the query.
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
			if values, parseErr := url.ParseQuery(string(body)); parseErr == nil {
				code = values.Get("code")
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(loginCallbackFailedHTML))
		return
	}
	select {
	case s.codes <- code:
	default:
	}
	_, _ = w.Write([]byte(loginCallbackSuccessHTML))
}

func portalRedirect(s *loginSession, raw string) (string, error) {
	u, err := url.Parse(raw)
	portal, perr := url.Parse(s.cfg.PortalHost)
	if err != nil || perr != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" ||
		!strings.EqualFold(u.Host, portal.Host) || len(u.Query()["ticket_id"]) != 1 || u.Query().Get("ticket_id") != s.ticket {
		return "", errors.New("invalid portal redirect")
	}
	return u.String(), nil
}

// loginMetadata is the polling context handed back to the CPA host so the host
// can re-supply it on every auth.login.poll (the host owns the state string and
// a plugin cannot recover the session from it alone).
func (s *loginSession) loginMetadata() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"session_id":   s.id,
		"port":         s.port,
		"callback_url": fmt.Sprintf("http://%s:%d%s", s.callbackHost, s.port, s.cfg.RedirectPath),
		"expires_at":   s.expires.UTC().Format(time.RFC3339),
	}
}

// callbackHost is the host name the browser must use to reach this listener.
// Inside a container the browser cannot see 127.0.0.1, so the loopback default
// only works for a same-namespace deployment; CODEARTS_CALLBACK_HOST overrides
// it (e.g. the Docker host IP or a published hostname).
func callbackHostFor() string {
	if host := strings.TrimSpace(os.Getenv("CODEARTS_CALLBACK_HOST")); host != "" {
		return host
	}
	return "127.0.0.1"
}

// metadataSessionID digs the session id out of the host-supplied poll metadata.
// The host echoes back whatever StartLogin returned, so this is the same value
// that arrived as the state; the fallback keeps older hosts working.
func metadataSessionID(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range []string{"session_id", "state"} {
		if raw, ok := metadata[key]; ok {
			if value, isString := raw.(string); isString && value != "" {
				return value
			}
		}
	}
	return ""
}

// currentLoginSummary lets the panel discover the loopback port of the pending
// login. The panel needs the port to know which origin the portal will redirect
// the browser to; the port is otherwise only known inside the plugin.
func currentLoginSummary() map[string]any {
	loginSessions.Lock()
	pruneLogins()
	newest := ""
	var newestExpiry time.Time
	for id, s := range loginSessions.byID {
		if s.data != nil {
			continue
		}
		if s.expires.After(newestExpiry) {
			newest, newestExpiry = id, s.expires
		}
	}
	loginSessions.Unlock()
	if newest == "" {
		return map[string]any{}
	}
	s := lookupLogin(newest)
	if s == nil {
		return map[string]any{}
	}
	return s.loginMetadata()
}

// acceptCallback feeds a browser-observed callback into the session. The TUI
// cannot submit plugin callbacks (it posts an empty provider and the host
// rejects that with 400), so the panel ships the callback here instead.
func (s *loginSession) acceptCallback(callbackURL string) map[string]any {
	u, err := url.Parse(strings.TrimSpace(callbackURL))
	if err != nil {
		return map[string]any{"error": "invalid callback URL"}
	}
	q := u.Query()
	if secret := q.Get("secret"); secret != "" {
		s.mu.Lock()
		s.secret = secret
		s.mu.Unlock()
	}
	if redirect := q.Get("redirect"); redirect != "" && q.Get("code") == "" {
		next, errRedirect := portalRedirect(s, redirect)
		if errRedirect != nil {
			return map[string]any{"error": "invalid redirect target"}
		}
		return map[string]any{"status": "continue", "next_url": next}
	}
	code := q.Get("code")
	if code == "" {
		return map[string]any{"error": "callback URL carries no code"}
	}
	select {
	case s.codes <- code:
	default:
	}
	return map[string]any{"status": "code_received"}
}

// handleLoginStart answers auth.login.start. The host records the returned state
// and shows the URL; polling arrives later through auth.login.poll.
func handleLoginStart(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	s, err := newLoginSession(strings.TrimSpace(req.BaseURL))
	if err != nil {
		return nil, newPluginError("login_unavailable", http.StatusInternalServerError, "login initialization failed")
	}
	if err = s.listenLoopback(); err != nil {
		stopLogin(s)
		return nil, newPluginError("login_unavailable", http.StatusServiceUnavailable, "loopback callback unavailable on this host")
	}
	if err = registerLogin(s); err != nil {
		dropLogin(s.id)
		return nil, newPluginError("login_unavailable", http.StatusServiceUnavailable, err.Error())
	}
	loginURL := newUpstreamClient(30*time.Second).BuildAuthorizeURL(s.cfg, s.ticket, upstream.PKCEChallenge(s.verifier), "SHA-256", s.port)
	// Metadata travels back through the host on every poll, so the panel and the
	// host drive the same session without sharing process memory.
	return okEnvelope(map[string]any{
		"provider":   providerName,
		"url":        loginURL,
		"state":      s.id,
		"expires_at": s.expires.UTC(),
		"metadata":   s.loginMetadata(),
	}), nil
}

// ticketReady reports whether a polled ticket carries a usable login. The
// reference CLI keys off a non-empty user name, because the portal can publish
// the profile slightly before the security token is readable; requiring the
// token alone would keep the poll pending forever on a successful login.
func ticketReady(tok *upstream.TokenResponse) bool {
	if tok == nil {
		return false
	}
	if tok.UserName != "" || tok.UserID != "" {
		return true
	}
	return tok.Credentials.SecurityToken != ""
}

// handleLoginPoll answers auth.login.poll with the dual-channel strategy of the
// reference CLI: an authorization code delivered to the loopback callback is
// exchanged first; otherwise the portal ticket is polled with the portal-issued
// secret (which the first callback supplies).
func handleLoginPoll(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	s := lookupLogin(req.State)
	if s == nil {
		s = lookupLogin(metadataSessionID(req.Metadata))
	}
	if s == nil {
		return okEnvelope(map[string]any{"status": string(pluginapi.AuthLoginStatusError), "message": "login expired or unknown; start a new login"}), nil
	}
	s.mu.Lock()
	if s.data != nil {
		data := *s.data
		s.mu.Unlock()
		return loginSuccessEnvelope(data), nil
	}
	if s.busy || time.Since(s.lastPoll) < loginPollInterval {
		s.mu.Unlock()
		return okEnvelope(map[string]any{"status": string(pluginapi.AuthLoginStatusPending)}), nil
	}
	s.busy = true
	s.lastPoll = time.Now()
	s.mu.Unlock()

	var tok *upstream.TokenResponse
	var exchangeErr, pollErr error
	select {
	case code := <-s.codes:
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		tok, exchangeErr = exchangeLoginCode(ctx, s, code)
		cancel()
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tok, pollErr = pollLoginTicket(ctx, s)
		cancel()
		// A not-ready ticket is the normal "still waiting for the browser" case.
		// The reference CLI treats a non-empty UserName as the success signal
		// (the portal publishes the profile before the STS credentials settle),
		// so a transport error alone never turns into a visible failure.
		if pollErr != nil && !ticketReady(tok) {
			pollErr = nil
		}
	}
	s.mu.Lock()
	s.busy = false
	if ticketReady(tok) {
		if err := finishLogin(s, tok); err != nil {
			s.mu.Unlock()
			return okEnvelope(map[string]any{"status": string(pluginapi.AuthLoginStatusError), "message": err.Error()}), nil
		}
	}
	data := s.data
	s.mu.Unlock()
	if data != nil {
		return loginSuccessEnvelope(*data), nil
	}
	if exchangeErr != nil {
		return okEnvelope(map[string]any{"status": string(pluginapi.AuthLoginStatusError), "message": "authorization code exchange failed"}), nil
	}
	return okEnvelope(map[string]any{"status": string(pluginapi.AuthLoginStatusPending)}), nil
}

func loginSuccessEnvelope(data pluginAuthData) []byte {
	return okEnvelope(map[string]any{
		"status": string(pluginapi.AuthLoginStatusSuccess),
		"auth":   data,
		"auths":  []pluginAuthData{data},
	})
}

func finishLogin(s *loginSession, tok *upstream.TokenResponse) error {
	if tok == nil || tok.Credentials.SecurityToken == "" || tok.Credentials.AccessKeyID == "" || tok.Credentials.SecretAccessKey == "" {
		return errors.New("incomplete login credentials")
	}
	if tok.UserID == "" {
		cred := upstream.SignCredential{AccessKeyID: tok.Credentials.AccessKeyID, SecretAccessKey: tok.Credentials.SecretAccessKey, SecurityToken: tok.Credentials.SecurityToken}
		uid, name, domain, err := upstream.CallerIdentity(cred)
		if err != nil || uid == "" {
			uid, name, domain, _ = upstream.CurrentUser(cred)
		}
		if uid == "" {
			uid = refreshTokenSubject(tok.RefreshToken)
		}
		tok.UserID = uid
		if tok.UserName == "" {
			tok.UserName = name
		}
		if tok.DomainID == "" {
			tok.DomainID = domain
		}
	}
	if tok.UserID == "" {
		return errors.New("login identity unavailable")
	}
	a := New(tok.UserID, tok.UserName, tok.DomainID, tok.Credentials.SecurityToken, tok.Credentials.AccessKeyID, tok.Credentials.SecretAccessKey, tok.Credentials.Expiration, tok.RefreshToken, s.verifier, map[string]string(s.key))
	a.SetClientID(s.cfg.ClientID)
	storage, err := json.Marshal(a)
	if err != nil {
		return err
	}
	name := a.FileName()
	if name == "codearts-unknown.json" || strings.ContainsAny(name, "/\\:") {
		return errors.New("invalid login identity")
	}
	metadata, disabled := authMetadata(storage)
	data := authData(a, storage, metadata, disabled)
	data.FileName = name
	data.Metadata["label"] = data.Label
	data.Metadata["type"] = providerName
	data.Metadata["email"] = a.UserName
	if _, err = saveLoginAuth(map[string]any{"name": name, "auth": data}); err != nil {
		if _, retryErr := saveLoginAuth(map[string]any{"name": name, "json": json.RawMessage(storage)}); retryErr != nil {
			return errors.New("could not save login")
		}
	}
	s.data = &data
	// Let the browser response flush before the callback socket goes away.
	if s.listener != nil {
		time.AfterFunc(time.Second, func() { stopLogin(s) })
	}
	return nil
}

func refreshTokenSubject(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Sub
}

// importLoginCallback accepts a pasted callback URL (remote browser). The
// session is addressed by the login state the host hands back.
func importLoginCallback(state, callbackURL string) (map[string]any, int) {
	s := lookupLogin(state)
	if s == nil {
		return map[string]any{"error": "login expired or unknown; start a new login"}, http.StatusNotFound
	}
	result := s.acceptCallback(callbackURL)
	if _, failed := result["error"]; failed {
		return result, http.StatusBadRequest
	}
	return result, http.StatusOK
}

// writeCallbackFile writes the host's callback payload
// (<auth dir>/.oauth-<provider>-<state>.oauth = {"code","state","error"}).
func writeCallbackFile(authDir, provider, state, code, errorMessage string) (string, error) {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return "", errors.New("auth dir is empty")
	}
	if err := validLoginState(state); err != nil {
		return "", err
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(authDir, fmt.Sprintf(".oauth-%s-%s.oauth", strings.ToLower(provider), state))
	raw, err := json.Marshal(map[string]any{"state": state, "code": code, "error": errorMessage})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// loginCallbackPort reports the loopback port a pending login is listening on;
// used by the panel to explain where the browser callback lands.
func loginCallbackPort(state string) int {
	if s := lookupLogin(state); s != nil {
		return s.port
	}
	return 0
}
