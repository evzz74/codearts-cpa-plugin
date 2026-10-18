package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"
)

func TestOAuthLoopbackDualChannel(t *testing.T) {
	oldExchange, oldSave := exchangeLoginCode, saveLoginAuth
	defer func() { exchangeLoginCode, saveLoginAuth = oldExchange, oldSave }()
	var saved []byte
	saveLoginAuth = func(v any) (json.RawMessage, error) {
		body, _ := json.Marshal(v)
		saved = body
		return json.RawMessage(`{}`), nil
	}
	exchangeLoginCode = func(_ context.Context, s *loginSession, code string) (*upstream.TokenResponse, error) {
		if code != "valid" || s.verifier == "" || s.key["d"] == "" {
			t.Fatal("lost PKCE or DPoP context")
		}
		return &upstream.TokenResponse{UserID: "account1", RefreshToken: "refresh-secret", Credentials: upstream.Credentials{SecurityToken: "private-token", AccessKeyID: "ak", SecretAccessKey: "sk"}}, nil
	}
	result, err := handleLoginStart(nil)
	if err != nil {
		t.Fatal(err)
	}
	value := envelopeResult(t, result)
	raw, _ := json.Marshal(value)
	var start struct {
		URL      string `json:"url"`
		State    string `json:"state"`
		Callback string `json:"callback_url"`
	}
	if err := json.Unmarshal(raw, &start); err != nil {
		t.Fatal(err)
	}
	if err := validLoginState(start.State); err != nil {
		t.Fatalf("state rejected by the host validator: %q: %v", start.State, err)
	}
	s := lookupLogin(start.State)
	if s == nil {
		t.Fatal("missing session")
	}
	defer dropLogin(start.State)
	if s.port == 0 || s.listener == nil {
		t.Fatal("loopback callback was not started")
	}
	authorize, _ := url.Parse(start.URL)
	if authorize.Query().Get("port") != strconv.Itoa(s.port) || authorize.Query().Get("code_challenge") == "" {
		t.Fatalf("authorize URL missing callback port or challenge: %s", start.URL)
	}
	// The portal's first callback carries secret+redirect and must be answered
	// with a 307 back into the portal, otherwise the code never arrives.
	portal, _ := url.Parse(s.cfg.PortalHost)
	redirect := portal.Scheme + "://" + portal.Host + "/authorize?ticket_id=" + s.ticket
	first := fmt.Sprintf("http://127.0.0.1:%d%s?secret=portal-secret&redirect=%s", s.port, s.cfg.RedirectPath, url.QueryEscape(redirect))
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(first)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("portal handshake callback: %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != redirect {
		t.Fatalf("portal redirect target: %q", loc)
	}
	if s.secret != "portal-secret" {
		t.Fatal("portal secret was not adopted for ticket polling")
	}
	// The second callback delivers the code.
	second := fmt.Sprintf("http://127.0.0.1:%d%s?code=valid", s.port, s.cfg.RedirectPath)
	resp, err = http.Get(second)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("code callback: %d", resp.StatusCode)
	}
	// The callback only queues the code; auth.login.poll performs the exchange and
	// the host save, exactly as the host drives it.
	poll, _ := json.Marshal(map[string]string{"state": start.State})
	done, err := handleLoginPoll(poll)
	if err != nil {
		t.Fatal(err)
	}
	values := envelopeResult(t, done)
	if string(values["status"]) != `"success"` || len(values["auths"]) == 0 {
		t.Fatalf("poll did not deliver the auth: %s", done)
	}
	var hostSave struct {
		Auth struct {
			StorageJSON []byte         `json:"StorageJSON"`
			FileName    string         `json:"FileName"`
			Metadata    map[string]any `json:"Metadata"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(saved, &hostSave); err != nil {
		t.Fatalf("host save payload missing: %v", err)
	}
	document := hostSave.Auth.StorageJSON
	if !strings.Contains(string(document), "refresh-secret") || !strings.Contains(string(document), "dpop_private_key") {
		t.Fatalf("lost refresh binding in host save: %s", document)
	}
	if hostSave.Auth.FileName != "codearts-account1.json" || hostSave.Auth.Metadata["type"] != providerName {
		t.Fatalf("host save metadata: %+v", hostSave.Auth)
	}
}

func TestLoginTicketChannelWhenCodeNeverArrives(t *testing.T) {
	oldPoll, oldSave := pollLoginTicket, saveLoginAuth
	defer func() { pollLoginTicket, saveLoginAuth = oldPoll, oldSave }()
	saveLoginAuth = func(any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
	var sawSecret string
	pollLoginTicket = func(_ context.Context, s *loginSession) (*upstream.TokenResponse, error) {
		sawSecret = s.secret
		return &upstream.TokenResponse{UserName: "u", UserID: "ticket-user", RefreshToken: "r", Credentials: upstream.Credentials{SecurityToken: "tok", AccessKeyID: "ak", SecretAccessKey: "sk"}}, nil
	}
	oldInterval := loginPollInterval
	loginPollInterval = 0
	defer func() { loginPollInterval = oldInterval }()
	result, err := handleLoginStart(nil)
	if err != nil {
		t.Fatal(err)
	}
	value := envelopeResult(t, result)
	raw, _ := json.Marshal(value)
	var start struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(raw, &start)
	defer dropLogin(start.State)
	poll, _ := json.Marshal(map[string]string{"state": start.State})
	done, err := handleLoginPoll(poll)
	if err != nil {
		t.Fatal(err)
	}
	values := envelopeResult(t, done)
	if string(values["status"]) != `"success"` {
		t.Fatalf("ticket channel did not complete the login: %s", done)
	}
	if sawSecret == "" {
		t.Fatal("ticket polling sent an empty secret")
	}
}

func TestOAuthRedirectGuardAndManagementAuth(t *testing.T) {
	s := &loginSession{ticket: "ticket", port: 17666, cfg: upstream.DefaultLoginConfig(), expires: time.Now().Add(time.Minute)}
	portal, _ := url.Parse(s.cfg.PortalHost)
	redirect := portal.Scheme + "://" + portal.Host + "/authorize?ticket_id=ticket"
	if next, err := portalRedirect(s, redirect); err != nil || next != redirect {
		t.Fatalf("valid portal redirect rejected: %s %v", next, err)
	}
	if _, err := portalRedirect(s, strings.Replace(redirect, portal.Host, "evil.example", 1)); err == nil {
		t.Fatal("open redirect accepted")
	}
	if _, err := portalRedirect(s, strings.Replace(redirect, "ticket_id=ticket", "ticket_id=other", 1)); err == nil {
		t.Fatal("redirect with a foreign ticket accepted")
	}
	managementSettings.Lock()
	old := managementSettings.key
	managementSettings.key = "test-key"
	managementSettings.Unlock()
	defer func() { managementSettings.Lock(); managementSettings.key = old; managementSettings.Unlock() }()
	request := managementRequest{Headers: http.Header{}}
	if managementAuthorized(request) {
		t.Fatal("unauthenticated mutation permitted")
	}
	request.Headers.Set("Authorization", "Bearer test-key")
	if !managementAuthorized(request) {
		t.Fatal("valid key rejected")
	}
}
