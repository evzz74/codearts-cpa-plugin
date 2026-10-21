package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"
)

func TestAuthRefreshSchedule(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name   string
		expiry time.Time
		want   time.Time
	}{
		{"normal", now.Add(time.Hour), now.Add(55 * time.Minute)},
		{"short", now.Add(2 * time.Minute), now.Add(time.Minute)},
		{"expired", now.Add(-time.Minute), now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := refreshAfter(now, tc.expiry); !got.Equal(tc.want) {
				t.Fatalf("schedule = %s, want %s", got, tc.want)
			}
		})
	}
	expiry := now.Add(time.Hour)
	storage, _ := json.Marshal(map[string]any{"type": providerName, "cloud_dragon_token": "sts", "expiration": expiry.Format(time.RFC3339), "metadata": map[string]any{"expired": "2000-01-01T00:00:00Z"}})
	request, _ := json.Marshal(authParseRequest{RawJSON: storage, FileName: "codearts-test.json"})
	raw, err := handleParseAuth(request)
	if err != nil {
		t.Fatal(err)
	}
	var data pluginAuthData
	if err := json.Unmarshal(envelopeResult(t, raw)["auth"], &data); err != nil {
		t.Fatal(err)
	}
	if !data.NextRefreshAfter.Equal(expiry.Add(-5 * time.Minute)) {
		t.Fatalf("parse schedule = %s", data.NextRefreshAfter)
	}
	// CPA 7.2.30 expirationFromMap checks "expired" before all other keys.
	gotExpiry, err := time.Parse(time.RFC3339Nano, data.Metadata["expired"].(string))
	if err != nil || !gotExpiry.Equal(expiry) {
		t.Fatalf("host metadata expiry = %s, %v", gotExpiry, err)
	}
	interval := time.Duration(data.Metadata["refresh_interval_seconds"].(float64)) * time.Second
	if interval <= 0 || interval > 30*time.Minute || expiry.Sub(time.Now()) <= interval {
		t.Fatalf("unsafe host interval = %s", interval)
	}
	unknown := authData(&Auth{}, nil, nil, false)
	if !unknown.NextRefreshAfter.IsZero() {
		t.Fatal("unknown expiry invented a schedule")
	}
}

func TestRefreshDoesNotRetryTerminalOrPermanentErrors(t *testing.T) {
	previous, backoff := refreshCodeartsToken, refreshBackoff
	defer func() { refreshCodeartsToken, refreshBackoff = previous, backoff }()
	refreshBackoff = 0
	for _, failure := range []error{
		&upstream.RefreshTokenExpiredError{Status: 400, Message: "secret-terminal"},
		&upstream.ApiError{Status: 400, Message: "secret-permanent"},
		errors.New("secret-local"),
	} {
		calls := 0
		refreshCodeartsToken = func(context.Context, upstream.LoginConfig, *Auth) (*upstream.TokenResponse, error) {
			calls++
			return nil, failure
		}
		request, _ := json.Marshal(authRefreshRequest{StorageJSON: []byte(`{"cloud_dragon_token":"old","refresh_token":"first","code_verifier":"pkce"}`)})
		_, err := handleRefreshAuth(request)
		if err == nil || calls != 1 || strings.Contains(err.Error(), "secret-") {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
		var expired *upstream.RefreshTokenExpiredError
		if errors.As(failure, &expired) && (errorCode(err) != "refresh_token_expired" || rpcStatus(t, err) != http.StatusUnauthorized) {
			t.Fatalf("terminal error = %v", err)
		}
	}
}

func TestRefreshUsesLatestStorageAndPersistsOnlyFailedRotation(t *testing.T) {
	previousToken, previousList, previousGet, previousSave, backoff := refreshCodeartsToken, hostAuthList, hostAuthGet, hostAuthSave, refreshBackoff
	defer func() {
		refreshCodeartsToken, hostAuthList, hostAuthGet, hostAuthSave, refreshBackoff = previousToken, previousList, previousGet, previousSave, backoff
	}()
	refreshBackoff = 0
	for _, tc := range []struct {
		name                          string
		success, saveFails, readFails bool
	}{
		{name: "failure saves rotation"},
		{name: "save failure", saveFails: true},
		{name: "reread failure", readFails: true},
		{name: "success merges latest", success: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostAuthList = func() (json.RawMessage, error) {
				return json.RawMessage(`{"files":[{"id":"host-id","auth_index":"index","name":"codearts-current.json","provider":"codearts"}]}`), nil
			}
			current := []byte(`{"type":"codearts","cloud_dragon_token":"old-sts","access_key_id":"old-ak","secret_access_key":"old-sk","expiration":"2020-01-01T00:00:00Z","refresh_token":"latest","code_verifier":"latest-pkce","disabled":false,"disabled_models":["model"],"metadata":{"note":"latest"},"unknown":{"keep":true}}`)
			gets, saves, calls := 0, 0, 0
			hostAuthGet = func(index string) (json.RawMessage, error) {
				gets++
				if index != "index" {
					t.Fatalf("wrong account index %q", index)
				}
				if gets > 1 && tc.readFails {
					return nil, errors.New("secret-read")
				}
				return json.Marshal(map[string]any{"json": json.RawMessage(current)})
			}
			var saved []byte
			hostAuthSave = func(value any) (json.RawMessage, error) {
				saves++
				payload := value.(map[string]any)
				if payload["name"] != "codearts-current.json" {
					t.Fatal("saved wrong account")
				}
				saved = append([]byte(nil), payload["json"].(json.RawMessage)...)
				if tc.saveFails {
					return nil, errors.New("secret-save")
				}
				return json.RawMessage(`{}`), nil
			}
			refreshCodeartsToken = func(_ context.Context, _ upstream.LoginConfig, a *Auth) (*upstream.TokenResponse, error) {
				calls++
				if a.CodeVerifier != "latest-pkce" || (calls == 1 && a.RefreshToken != "latest") || (calls == 2 && a.RefreshToken != "rotated") {
					t.Fatal("refresh used stale credentials")
				}
				// Simulate an operator update before the post-refresh re-read.
				var err error
				current, err = mergeAccountFields(current, map[string]any{"disabled": true, "metadata": map[string]any{"note": "newest"}})
				if err != nil {
					t.Fatal(err)
				}
				if tc.success {
					return &upstream.TokenResponse{RefreshToken: "rotated", Credentials: upstream.Credentials{SecurityToken: "new-sts", AccessKeyID: "new-ak", SecretAccessKey: "new-sk", Expiration: "2030-01-01T00:00:00Z"}}, nil
				}
				if calls == 1 {
					return &upstream.TokenResponse{RefreshToken: "rotated"}, &upstream.ApiError{Status: 502}
				}
				return nil, &upstream.ApiError{Status: 503}
			}
			request, _ := json.Marshal(authRefreshRequest{AuthID: "host-id", StorageJSON: []byte(`{"cloud_dragon_token":"stale","refresh_token":"stale","code_verifier":"stale"}`), Metadata: map[string]any{"disabled": false, "note": "stale"}})
			raw, err := handleRefreshAuth(request)
			if tc.success {
				if err != nil {
					t.Fatal(err)
				}
				if saves != 0 {
					t.Fatal("success should be saved by caller")
				}
				var data pluginAuthData
				result := envelopeResult(t, raw)
				if err := json.Unmarshal(result["auth"], &data); err != nil {
					t.Fatal(err)
				}
				if !data.Disabled || data.Metadata["note"] != "newest" {
					t.Fatal("latest metadata/disabled lost")
				}
				var next time.Time
				if err := json.Unmarshal(result["next_refresh_after"], &next); err != nil || !next.Equal(data.NextRefreshAfter) {
					t.Fatal("refresh gates differ")
				}
				saved = data.StorageJSON
			} else {
				if err == nil || len(raw) != 0 || strings.Contains(err.Error(), "secret-") {
					t.Fatalf("failure falsely succeeded/leaked: %v", err)
				}
				if tc.saveFails || tc.readFails {
					if errorCode(err) != "refresh_save_failed" {
						t.Fatalf("save failure hidden: %v", err)
					}
					return
				}
				if saves != 1 || calls != refreshAttempts {
					t.Fatalf("saves=%d calls=%d", saves, calls)
				}
			}
			var fields, latest map[string]any
			_ = json.Unmarshal(saved, &fields)
			_ = json.Unmarshal(current, &latest)
			if fields["refresh_token"] != "rotated" || fields["disabled"] != true || !reflect.DeepEqual(fields["disabled_models"], latest["disabled_models"]) || !reflect.DeepEqual(fields["unknown"], latest["unknown"]) {
				t.Fatal("rotation or unrelated fields lost")
			}
			if !tc.success {
				latest["refresh_token"] = "rotated"
				if !reflect.DeepEqual(fields, latest) {
					t.Fatal("failed refresh modified fields other than refresh_token")
				}
			}
		})
	}
}

func TestConcurrentRefreshReadsRotationAfterAccountLock(t *testing.T) {
	previousList, previousGet, previousSave, previousToken, backoff := hostAuthList, hostAuthGet, hostAuthSave, refreshCodeartsToken, refreshBackoff
	defer func() {
		hostAuthList, hostAuthGet, hostAuthSave, refreshCodeartsToken, refreshBackoff = previousList, previousGet, previousSave, previousToken, backoff
	}()
	refreshBackoff = 0
	current := json.RawMessage(`{"type":"codearts","cloud_dragon_token":"old","refresh_token":"initial","code_verifier":"pkce"}`)
	hostAuthList = func() (json.RawMessage, error) {
		return json.RawMessage(`{"files":[{"id":"concurrent-id","auth_index":"index","name":"codearts-concurrent.json"}]}`), nil
	}
	hostAuthGet = func(string) (json.RawMessage, error) { return json.Marshal(map[string]any{"json": current}) }
	hostAuthSave = func(value any) (json.RawMessage, error) {
		current = value.(map[string]any)["json"].(json.RawMessage)
		return json.RawMessage(`{}`), nil
	}
	started, secondLaunched := make(chan struct{}), make(chan struct{})
	calls := 0
	refreshCodeartsToken = func(_ context.Context, _ upstream.LoginConfig, a *Auth) (*upstream.TokenResponse, error) {
		calls++
		if calls == 1 {
			close(started)
			<-secondLaunched
			if a.RefreshToken != "initial" {
				return nil, errors.New("unexpected initial token")
			}
			return &upstream.TokenResponse{RefreshToken: "rotated"}, &upstream.ApiError{Status: 502}
		}
		if a.RefreshToken != "rotated" {
			return nil, errors.New("concurrent refresh used stale token")
		}
		if calls == 2 {
			return nil, &upstream.ApiError{Status: 503}
		}
		return &upstream.TokenResponse{Credentials: upstream.Credentials{SecurityToken: "new", AccessKeyID: "ak", SecretAccessKey: "sk"}}, nil
	}
	request, _ := json.Marshal(authRefreshRequest{AuthID: "concurrent-id", StorageJSON: []byte(`{"cloud_dragon_token":"stale","refresh_token":"stale","code_verifier":"pkce"}`)})
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := handleRefreshAuth(request); first <- err }()
	<-started
	go func() { close(secondLaunched); _, err := handleRefreshAuth(request); second <- err }()
	if err := <-first; err == nil {
		t.Fatal("first refresh falsely succeeded")
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("refresh calls = %d", calls)
	}
}

func TestRefreshWithoutIDDoesNotAccessHostStore(t *testing.T) {
	previousList, previousGet, previousSave, previousToken, backoff := hostAuthList, hostAuthGet, hostAuthSave, refreshCodeartsToken, refreshBackoff
	defer func() {
		hostAuthList, hostAuthGet, hostAuthSave, refreshCodeartsToken, refreshBackoff = previousList, previousGet, previousSave, previousToken, backoff
	}()
	refreshBackoff = 0
	hostAuthList = func() (json.RawMessage, error) { t.Fatal("legacy refresh listed host accounts"); return nil, nil }
	hostAuthGet = func(string) (json.RawMessage, error) { t.Fatal("legacy refresh read host account"); return nil, nil }
	hostAuthSave = func(any) (json.RawMessage, error) { t.Fatal("legacy refresh saved host account"); return nil, nil }
	calls := 0
	refreshCodeartsToken = func(_ context.Context, _ upstream.LoginConfig, a *Auth) (*upstream.TokenResponse, error) {
		calls++
		if calls == 1 {
			return &upstream.TokenResponse{RefreshToken: "rotated"}, &upstream.ApiError{Status: 502}
		}
		if a.RefreshToken != "rotated" {
			t.Fatal("retry lost rotation")
		}
		return nil, &upstream.RefreshTokenExpiredError{Status: 400, Message: "secret-terminal"}
	}
	request, _ := json.Marshal(authRefreshRequest{StorageJSON: []byte(`{"cloud_dragon_token":"old","refresh_token":"old","code_verifier":"pkce"}`)})
	if raw, err := handleRefreshAuth(request); err == nil || len(raw) != 0 || errorCode(err) != "refresh_token_expired" || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestRefreshUnknownIDNeverFallsBackToRequestStorage(t *testing.T) {
	previousList, previousGet, previousSave, previousToken := hostAuthList, hostAuthGet, hostAuthSave, refreshCodeartsToken
	defer func() {
		hostAuthList, hostAuthGet, hostAuthSave, refreshCodeartsToken = previousList, previousGet, previousSave, previousToken
	}()
	hostAuthList = func() (json.RawMessage, error) {
		return json.RawMessage(`{"files":[{"id":"other","auth_index":"requested","name":"codearts-other.json","provider":"codearts"}]}`), nil
	}
	hostAuthGet = func(string) (json.RawMessage, error) { t.Fatal("unknown ID read another account"); return nil, nil }
	hostAuthSave = func(any) (json.RawMessage, error) { t.Fatal("unknown ID saved another account"); return nil, nil }
	refreshCodeartsToken = func(context.Context, upstream.LoginConfig, *Auth) (*upstream.TokenResponse, error) {
		t.Fatal("unknown ID used stale token")
		return nil, nil
	}
	request, _ := json.Marshal(authRefreshRequest{AuthID: "requested", StorageJSON: []byte(`{"cloud_dragon_token":"old","refresh_token":"old","code_verifier":"pkce"}`)})
	if _, err := handleRefreshAuth(request); err == nil {
		t.Fatal("unknown ID accepted")
	}
}
