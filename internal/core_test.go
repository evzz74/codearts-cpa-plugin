package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func envelopeResult(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	result, err := hostUnwrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(result, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestRegistrationAndUnknownMethod(t *testing.T) {
	raw, err := handleMethod("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	result := envelopeResult(t, raw)
	if string(result["schema_version"]) != "1" {
		t.Fatalf("schema: %s", result["schema_version"])
	}
	var capabilities map[string]any
	if err := json.Unmarshal(result["capabilities"], &capabilities); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model_provider", "auth_provider", "executor", "management_api"} {
		if capabilities[name] != true {
			t.Errorf("missing %s capability", name)
		}
	}
	raw, err = handleMethod("unknown", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hostUnwrap(raw); err == nil {
		t.Fatal("unknown method accepted")
	}
}

func TestParseAuthOwnershipAndIdentity(t *testing.T) {
	storage := []byte(`{"type":"codearts","user_id":"u1","user_name":"Ada","cloud_dragon_token":"token","disabled":true,"metadata":{"note":"keep"}}`)
	request, _ := json.Marshal(authParseRequest{FileName: "legacy-name.json", RawJSON: storage})
	raw, err := handleParseAuth(request)
	if err != nil {
		t.Fatal(err)
	}
	result := envelopeResult(t, raw)
	if string(result["handled"]) != "true" {
		t.Fatal("not handled")
	}
	var data pluginAuthData
	if err := json.Unmarshal(result["auth"], &data); err != nil {
		t.Fatal(err)
	}
	if data.ID != "" || data.FileName != "legacy-name.json" || !data.Disabled || data.Metadata["note"] != "keep" || !reflect.DeepEqual(data.StorageJSON, storage) {
		t.Fatalf("auth not preserved: %+v", data)
	}
	other, _ := json.Marshal(authParseRequest{FileName: "codearts-u1.json", RawJSON: []byte(`{"type":"other","cloud_dragon_token":"token"}`)})
	raw, err = handleParseAuth(other)
	if err != nil {
		t.Fatal(err)
	}
	if string(envelopeResult(t, raw)["handled"]) != "false" {
		t.Fatal("claimed foreign credential")
	}
}

func TestRefreshPreservesStorageAndMetadata(t *testing.T) {
	previous := refreshCodeartsToken
	defer func() { refreshCodeartsToken = previous }()
	refreshCodeartsToken = func(_ context.Context, cfg upstream.LoginConfig, acct *Auth) (*upstream.TokenResponse, error) {
		if cfg.ClientID != "custom" || acct.CodeVerifier != "pkce" || acct.DPoPPrivateKey["d"] != "private" {
			t.Fatalf("lost bound credentials: %+v %+v", cfg, acct)
		}
		return &upstream.TokenResponse{RefreshToken: "rotated", Credentials: upstream.Credentials{SecurityToken: "new", AccessKeyID: "ak", SecretAccessKey: "sk", Expiration: "2030-01-01T00:00:00Z"}}, nil
	}
	storage := []byte(`{"type":"codearts","user_id":"u1","cloud_dragon_token":"old","refresh_token":"old-refresh","code_verifier":"pkce","client_id":"custom","dpop_private_key":{"d":"private"},"custom_field":{"keep":true},"disabled":true,"metadata":{"note":"original"}}`)
	request, _ := json.Marshal(authRefreshRequest{StorageJSON: storage, Metadata: map[string]any{"extra": "host"}})
	raw, err := handleRefreshAuth(request)
	if err != nil {
		t.Fatal(err)
	}
	envelope := envelopeResult(t, raw)
	var data pluginAuthData
	if err := json.Unmarshal(envelope["auth"], &data); err != nil {
		t.Fatal(err)
	}
	if data.FileName != "" || data.ID != "" || !data.Disabled || data.Metadata["note"] != "original" || data.Metadata["extra"] != "host" {
		t.Fatalf("metadata: %+v", data)
	}
	if _, ok := envelope["next_refresh_after"]; !ok {
		t.Fatalf("next_refresh_after missing: %s", raw)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data.StorageJSON, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["refresh_token"]) != `"rotated"` || string(fields["cloud_dragon_token"]) != `"new"` || string(fields["custom_field"]) != `{"keep":true}` {
		t.Fatalf("storage: %s", data.StorageJSON)
	}
	refreshCodeartsToken = func(context.Context, upstream.LoginConfig, *Auth) (*upstream.TokenResponse, error) {
		return nil, errors.New("offline")
	}
	_, err = handleRefreshAuth(request)
	if err == nil {
		t.Fatal("refresh failure swallowed")
	}
	if status := rpcStatus(t, err); status != 502 {
		t.Fatalf("refresh outage status = %d, want 502", status)
	}
}

func rpcStatus(t *testing.T, err error) int {
	t.Helper()
	var envelope pluginabi.Envelope
	if unmarshalErr := json.Unmarshal(rpcErrorEnvelope(err), &envelope); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if envelope.OK || envelope.Error == nil {
		t.Fatalf("expected a failed envelope: %s", rpcErrorEnvelope(err))
	}
	return envelope.Error.HTTPStatus
}

func TestRefreshRetriesAndKeepsRotatedToken(t *testing.T) {
	previousToken, previousBackoff := refreshCodeartsToken, refreshBackoff
	defer func() { refreshCodeartsToken, refreshBackoff = previousToken, previousBackoff }()
	refreshBackoff = time.Millisecond
	var seen []string
	refreshCodeartsToken = func(context.Context, upstream.LoginConfig, *Auth) (*upstream.TokenResponse, error) {
		return nil, errors.New("upstream unavailable")
	}
	storage := []byte(`{"type":"codearts","user_id":"u1","cloud_dragon_token":"old","refresh_token":"first","code_verifier":"pkce"}`)
	acct, err := authFromStorage(storage)
	if err != nil {
		t.Fatal(err)
	}
	// First attempt rotates the token but fails; the retry must reuse the new one.
	refreshCodeartsToken = func(_ context.Context, _ upstream.LoginConfig, a *Auth) (*upstream.TokenResponse, error) {
		seen = append(seen, a.RefreshToken)
		if len(seen) == 1 {
			return &upstream.TokenResponse{RefreshToken: "second"}, &upstream.ApiError{Status: http.StatusBadGateway, Message: "sts unavailable"}
		}
		return &upstream.TokenResponse{RefreshToken: "second", Credentials: upstream.Credentials{SecurityToken: "new", AccessKeyID: "ak", SecretAccessKey: "sk"}}, nil
	}
	token, err := refreshWithRetry(context.Background(), upstream.DefaultLoginConfig(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "first" || seen[1] != "second" {
		t.Fatalf("unexpected refresh sequence: %v", seen)
	}
	if token == nil || token.RefreshToken != "second" || acct.RefreshToken != "second" {
		t.Fatalf("rotated token lost: %+v acct=%q", token, acct.RefreshToken)
	}
	// Both attempts failing yields the upstream outage status, not a panic.
	seen = nil
	refreshCodeartsToken = func(context.Context, upstream.LoginConfig, *Auth) (*upstream.TokenResponse, error) {
		seen = append(seen, "x")
		return nil, &upstream.ApiError{Status: http.StatusServiceUnavailable, Message: "down"}
	}
	request, _ := json.Marshal(authRefreshRequest{StorageJSON: storage})
	if _, err = handleRefreshAuth(request); err == nil || len(seen) != refreshAttempts {
		t.Fatalf("attempts=%d err=%v", len(seen), err)
	}
	if status := rpcStatus(t, err); status != http.StatusBadGateway {
		t.Fatalf("status = %d", status)
	}
}

func TestAuthErrorsCarryHostStatus(t *testing.T) {
	previous := refreshCodeartsToken
	defer func() { refreshCodeartsToken = previous }()
	for _, tc := range []struct {
		name    string
		storage string
		status  int
		code    string
	}{
		{"missing token", `{"user_id":"u1"}`, http.StatusBadRequest, "auth_invalid"},
		{"malformed", `not-json`, http.StatusBadRequest, "auth_invalid"},
	} {
		request, _ := json.Marshal(authRefreshRequest{StorageJSON: []byte(tc.storage)})
		_, err := handleRefreshAuth(request)
		if err == nil {
			t.Fatalf("%s: refresh accepted", tc.name)
		}
		if status := rpcStatus(t, err); status != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.name, status, tc.status)
		}
		if code := errorCode(err); code != tc.code {
			t.Errorf("%s: code = %q, want %q", tc.name, code, tc.code)
		}
	}
	// Executor paths classify the same way so the host can disable the auth.
	raw, _ := json.Marshal(executorRequest{Model: "GLM-5.2", StorageJSON: []byte(`{"user_id":"u1","cloud_dragon_token":"token"}`), Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	if _, err := handleExecute(raw); err == nil {
		t.Fatal("executor accepted a credential without signing keys")
	} else if status := rpcStatus(t, err); status != http.StatusForbidden {
		t.Fatalf("executor status = %d", status)
	}
}

func TestSDKWireFormatAndAlias(t *testing.T) {
	storage := []byte(`{"type":"codearts","user_id":"u1","cloud_dragon_token":"token"}`)
	request, err := json.Marshal(pluginapi.AuthParseRequest{RawJSON: storage, FileName: "codearts-u1.json"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := handleParseAuth(request)
	if err != nil {
		t.Fatal(err)
	}
	var parsed pluginapi.AuthParseResponse
	if err := json.Unmarshal(envelopeResult(t, response)["auth"], &parsed.Auth); err != nil || string(parsed.Auth.StorageJSON) != string(storage) {
		t.Fatalf("SDK auth wire round-trip: %+v %v", parsed.Auth, err)
	}
	modelsReq, _ := json.Marshal(pluginapi.StaticModelRequest{Host: pluginapi.HostConfigSummary{
		OAuthModelAlias: map[string][]pluginapi.ModelAlias{providerName: {{Name: "GLM-5.2", Alias: "my-glm"}}},
	}})
	if _, err := handleModelStatic(modelsReq); err != nil {
		t.Fatal(err)
	}
	if got := upstreamModelName("my-glm"); got != "GLM-5.2" {
		t.Fatalf("alias resolved to %q", got)
	}
}

func TestModelsPerAuthAndStatic(t *testing.T) {
	previous := discoverModels
	defer func() { discoverModels = previous }()
	discoverModels = func(acct *Auth) ([]upstream.ModelInfo, error) {
		if acct.UserID != "u2" {
			t.Fatal("wrong account")
		}
		return []upstream.ModelInfo{{ID: "model-for-u2", ContextWindow: 1000, MaxTokens: 32}}, nil
	}
	request, _ := json.Marshal(map[string]any{"StorageJSON": []byte(`{"user_id":"u2","cloud_dragon_token":"token"}`)})
	raw, err := handleModelForAuth(request)
	if err != nil {
		t.Fatal(err)
	}
	var models []pluginModelInfo
	if err := json.Unmarshal(envelopeResult(t, raw)["models"], &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "model-for-u2" || models[0].ContextLength != 1000 {
		t.Fatalf("models: %+v", models)
	}
	raw, err = handleModelStatic(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(envelopeResult(t, raw)["models"], &models); err != nil || len(models) == 0 {
		t.Fatalf("static models: %+v %v", models, err)
	}
}
