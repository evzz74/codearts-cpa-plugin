package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/upstream"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// jsonMap decodes an envelope value into a plain map for assertions.
func jsonMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	value := envelopeResult(t, raw)
	joined, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(joined, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// responseBody decodes the ManagementResponse envelope the resource handlers
// return: Body crosses the RPC as base64, so it must be unwrapped before the
// payload can be inspected.
func responseBody(t *testing.T, raw []byte) (int, map[string]any) {
	t.Helper()
	value := jsonMap(t, raw)
	status, _ := value["StatusCode"].(float64)
	encoded, _ := value["Body"].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("body is not base64: %v", err)
	}
	var out map[string]any
	if len(decoded) > 0 {
		if err := json.Unmarshal(decoded, &out); err != nil {
			t.Fatalf("body is not json: %s", decoded)
		}
	}
	return int(status), out
}

// TestBootstrapPublishesHostLoginRoutes pins the contract the panel relies on:
// the host's standard plugin login route and status endpoint, plus the
// unauthenticated resource bridge, all without a management key.
func TestBootstrapPublishesHostLoginRoutes(t *testing.T) {
	status, value := responseBody(t, managementBootstrap("/bootstrap", nil))
	if status != http.StatusOK {
		t.Fatalf("bootstrap status = %d", status)
	}
	if value["provider"] != providerName {
		t.Fatalf("provider = %v", value["provider"])
	}
	authURL, _ := value["auth_url"].(string)
	if !strings.HasSuffix(authURL, "/management/"+providerName+"-auth-url") {
		t.Fatalf("auth_url = %q", authURL)
	}
	statusURL, _ := value["auth_status_url"].(string)
	if !strings.HasSuffix(statusURL, "/management/get-auth-status") {
		t.Fatalf("auth_status_url = %q", statusURL)
	}
	bridge, _ := value["callback_url"].(string)
	if !strings.HasPrefix(bridge, "/v0/resource/plugins/codearts/") {
		t.Fatalf("callback bridge must live on the unauthenticated resource surface: %q", bridge)
	}
	if strings.Contains(authURL, "oauth-callback") {
		t.Fatal("panel must not route through the host oauth-callback endpoint")
	}
}

// TestPollRecoversSessionFromHostMetadata mirrors the host contract: it echoes
// the StartLogin metadata back on every poll, so the plugin can find the session
// even when the state it stored is not what the plugin generated.
func TestPollRecoversSessionFromHostMetadata(t *testing.T) {
	oldPoll, oldSave := pollLoginTicket, saveLoginAuth
	defer func() { pollLoginTicket, saveLoginAuth = oldPoll, oldSave }()
	saveLoginAuth = func(any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
	pollLoginTicket = func(_ context.Context, _ *loginSession) (*upstream.TokenResponse, error) {
		return &upstream.TokenResponse{UserID: "meta-user", RefreshToken: "r", Credentials: upstream.Credentials{SecurityToken: "tok", AccessKeyID: "ak", SecretAccessKey: "sk"}}, nil
	}
	oldInterval := loginPollInterval
	loginPollInterval = 0
	defer func() { loginPollInterval = oldInterval }()

	start := jsonMap(t, mustStart(t))
	state, _ := start["state"].(string)
	defer dropLogin(state)
	metadata, ok := start["metadata"].(map[string]any)
	if !ok || metadata["session_id"] != state {
		t.Fatalf("start metadata must carry the session id: %v", start["metadata"])
	}
	if port, _ := metadata["port"].(float64); port <= 0 {
		t.Fatalf("start metadata must publish the loopback port: %v", metadata["port"])
	}

	// The host hands back a state the plugin never generated, only the metadata.
	poll, _ := json.Marshal(map[string]any{"state": "host-generated-state", "metadata": metadata})
	done, err := handleLoginPoll(poll)
	if err != nil {
		t.Fatal(err)
	}
	if got := jsonMap(t, done)["status"]; got != string(pluginapi.AuthLoginStatusSuccess) {
		t.Fatalf("poll could not recover the session from host metadata: %s", done)
	}
}

// TestResourceBridgeFeedsCallback verifies the unauthenticated bridge path the
// browser uses when it observes the loopback callback itself.
func TestResourceBridgeFeedsCallback(t *testing.T) {
	start := jsonMap(t, mustStart(t))
	state, _ := start["state"].(string)
	defer dropLogin(state)
	s := lookupLogin(state)
	if s == nil {
		t.Fatal("missing session")
	}
	query := url.Values{}
	query.Set("state", state)
	query.Set("callback_url", "http://127.0.0.1:9/oauth/callback?code=bridge-code")
	status, value := responseBody(t, managementBootstrap("/post/callback", query))
	if status != http.StatusOK {
		t.Fatalf("bridge rejected the callback: %v", value)
	}
	if value["status"] != "code_received" {
		t.Fatalf("bridge status = %v", value["status"])
	}
	select {
	case code := <-s.codes:
		if code != "bridge-code" {
			t.Fatalf("code = %q", code)
		}
	default:
		t.Fatal("bridge did not queue the code")
	}
}

// TestTicketReadyMatchesReferenceCLI pins the success predicate to the reference
// CLI: a published profile counts as a completed login even before the security
// token is readable, otherwise polling reports pending forever.
func TestTicketReadyMatchesReferenceCLI(t *testing.T) {
	if ticketReady(nil) {
		t.Fatal("nil ticket reported ready")
	}
	if ticketReady(&upstream.TokenResponse{}) {
		t.Fatal("empty ticket reported ready")
	}
	if !ticketReady(&upstream.TokenResponse{UserName: "someone"}) {
		t.Fatal("ticket with a user name must be ready (reference CLI behaviour)")
	}
	if !ticketReady(&upstream.TokenResponse{Credentials: upstream.Credentials{SecurityToken: "tok"}}) {
		t.Fatal("ticket with a security token must be ready")
	}
}

// TestPollIntervalBelowHostCadence guards against a throttle that outlasts the
// host's own polling, which made every tick return pending.
func TestPollIntervalBelowHostCadence(t *testing.T) {
	if loginPollInterval >= 2*time.Second {
		t.Fatalf("loginPollInterval %v is not below the host's 2s cadence", loginPollInterval)
	}
}

func mustStart(t *testing.T) []byte {
	t.Helper()
	result, err := handleLoginStart(nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
