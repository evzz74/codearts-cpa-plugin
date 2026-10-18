package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// pluginError lets a handler attach an HTTP status to an RPC failure so the host
// can treat disabled, expired or missing credentials differently from outages.
type pluginError struct {
	code    string
	message string
	status  int
}

func (e *pluginError) Error() string { return e.message }

// newPluginError keeps upstream details out of messages the host may surface.
func newPluginError(code string, status int, message string) error {
	return &pluginError{code: code, message: message, status: status}
}

// withStatus wraps err with an HTTP status unless it already carries one.
func withStatus(err error, status int) error {
	if err == nil {
		return nil
	}
	if typed, ok := err.(*pluginError); ok {
		if typed.status != 0 {
			return typed
		}
		return &pluginError{code: typed.code, message: typed.message, status: status}
	}
	return &pluginError{code: "plugin_error", message: err.Error(), status: status}
}

// authStorageError classifies why a stored credential cannot be used. A missing
// or malformed document is a property of the auth file, not of the request, so
// the host may disable or re-login instead of retrying the request.
func authStorageError(err error) error {
	message := err.Error()
	switch {
	case strings.HasPrefix(message, "missing cloud_dragon_token"):
		return newPluginError("auth_unusable", http.StatusForbidden, message)
	case strings.HasPrefix(message, "missing account signing credentials"):
		return newPluginError("auth_unusable", http.StatusForbidden, message)
	default:
		return newPluginError("auth_invalid", http.StatusBadRequest, message)
	}
}

func errorCode(err error) string {
	if typed, ok := err.(*pluginError); ok && typed.code != "" {
		return typed.code
	}
	return "plugin_error"
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func rpcErrorEnvelope(err error) []byte {
	if err == nil {
		return errorEnvelope("plugin_error", "unknown plugin error")
	}
	envelope := pluginabi.Envelope{OK: false, Error: &pluginabi.Error{
		Code:       errorCode(err),
		Message:    err.Error(),
		Retryable:  false,
		HTTPStatus: 0,
	}}
	if typed, ok := err.(*pluginError); ok && typed.status != 0 {
		envelope.Error.HTTPStatus = typed.status
		envelope.Error.Retryable = retryableStatus(typed.status)
	} else {
		envelope.Error.Retryable = true
	}
	raw, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		// The message is an error string, so marshalling only fails if the
		// envelope shape is broken; keep a minimal, still-valid fallback.
		return errorEnvelope("plugin_error", "plugin error")
	}
	return raw
}

// refreshCodeartsToken is replaceable in tests without contacting Huawei Cloud.
var refreshCodeartsToken = func(ctx context.Context, cfg upstream.LoginConfig, acct *Auth) (*upstream.TokenResponse, error) {
	return newUpstreamClient(30*time.Second).RefreshToken(ctx, cfg, acct.RefreshToken, acct.CodeVerifier, upstream.DPoPPrivateJWK(acct.DPoPPrivateKey))
}

// refreshAttempts tolerates one transient STS failure; the rotated refresh token
// is released after either attempt so the host's saved copy stays current.
const refreshAttempts = 2

var refreshBackoff = 300 * time.Millisecond

// refreshWithRetry calls the refresh endpoint, retrying once for transient
// failures. The upstream rotates refresh_token even when issuing STS credentials
// fails, so a rotated value from a failed attempt is carried into the next one.
func refreshWithRetry(ctx context.Context, cfg upstream.LoginConfig, acct *Auth) (*upstream.TokenResponse, error) {
	var lastErr error
	for try := 0; try < refreshAttempts; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return nil, lastErr
			case <-time.After(refreshBackoff):
			}
		}
		token, err := refreshCodeartsToken(ctx, cfg, acct)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if token == nil {
			return nil, newPluginError("refresh_unavailable", http.StatusBadGateway, "refresh: missing token response")
		}
		if token.RefreshToken != "" {
			// The upstream rotates refresh_token; record it so a caller that
			// retries this same Auth object keeps using the live token.
			acct.RefreshToken = token.RefreshToken
		}
		return token, nil
	}
	return nil, lastErr
}

// The host auth store RPCs are package-level variables so tests can stub them.
var (
	hostAuthList = func() (json.RawMessage, error) {
		raw, err := hostCall("host.auth.list", nil)
		if err != nil {
			return nil, newPluginError("auth_store_unavailable", http.StatusBadGateway, "auth store unavailable")
		}
		result, err := hostUnwrap(raw)
		if err != nil {
			return nil, newPluginError("auth_store_unavailable", http.StatusBadGateway, "auth store unavailable")
		}
		return result, nil
	}
	hostAuthGet = func(authIndex string) (json.RawMessage, error) {
		body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
		raw, err := hostCall("host.auth.get", body)
		if err != nil {
			return nil, newPluginError("auth_not_found", http.StatusNotFound, "account not found in the host auth store")
		}
		result, err := hostUnwrap(raw)
		if err != nil {
			return nil, newPluginError("auth_not_found", http.StatusNotFound, "account not found in the host auth store")
		}
		return result, nil
	}
	hostAuthSave = func(v any) (json.RawMessage, error) {
		body, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encode auth save: %w", err)
		}
		raw, err := hostCall("host.auth.save", body)
		if err != nil {
			return nil, newPluginError("auth_store_unavailable", http.StatusBadGateway, "auth store unavailable")
		}
		result, err := hostUnwrap(raw)
		if err != nil {
			return nil, newPluginError("auth_store_unavailable", http.StatusBadGateway, "auth store unavailable")
		}
		return result, nil
	}
)

// authFiles lists CodeArts auth files sorted by name so responses stay stable.
func sortedAuthFiles(files []accountFile) []accountFile {
	out := append([]accountFile(nil), files...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
