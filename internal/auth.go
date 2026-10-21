package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type pluginAuthData = pluginapi.AuthData

type authParseRequest = pluginapi.AuthParseRequest

type authRefreshRequest = pluginapi.AuthRefreshRequest

func ownedAuth(fileName, provider string, storage []byte) bool {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(storage, &probe) != nil {
		return false
	}
	if probe.Type != "" {
		return strings.EqualFold(probe.Type, providerName)
	}
	name := strings.ToLower(strings.TrimSpace(fileName))
	return strings.EqualFold(provider, providerName) || name == "codearts.json" ||
		(strings.HasPrefix(name, "codearts-") && strings.HasSuffix(name, ".json") && len(name) > len("codearts-.json"))
}

func authFromStorage(raw []byte) (*Auth, error) {
	var acct Auth
	if err := json.Unmarshal(raw, &acct); err != nil {
		return nil, err
	}
	if acct.CloudDragonTok == "" {
		return nil, fmt.Errorf("missing cloud_dragon_token")
	}
	return &acct, nil
}

func authMetadata(storage []byte) (map[string]any, bool) {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(storage, &fields)
	metadata := map[string]any{}
	_ = json.Unmarshal(fields["metadata"], &metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	disabled := false
	_ = json.Unmarshal(fields["disabled"], &disabled)
	if value, ok := metadata["disabled"].(bool); ok {
		disabled = value
	}
	return metadata, disabled
}

func authData(acct *Auth, storage []byte, metadata map[string]any, disabled bool) pluginAuthData {
	if metadata == nil {
		metadata = map[string]any{}
	}
	// CPA overlays metadata onto StorageJSON when persisting; stale copies must not win.
	var stored map[string]json.RawMessage
	if json.Unmarshal(storage, &stored) == nil {
		for key := range stored {
			delete(metadata, key)
		}
	}
	metadata["type"] = providerName
	label := acct.UserName
	if label == "" {
		label = acct.UserID
	}
	data := pluginAuthData{Provider: providerName, Label: label, Disabled: disabled, StorageJSON: storage, Metadata: metadata}
	if expires := acct.ExpiresAt(); !expires.IsZero() {
		now := time.Now().UTC()
		data.NextRefreshAfter = refreshAfter(now, expires)
		// CPA clears the refresh gate after success; its interval fallback also needs a recognized expiry.
		metadata["expired"] = expires.UTC().Format(time.RFC3339Nano)
		interval := expires.Sub(now) / 2 / time.Second
		if interval < 1 {
			interval = 1
		}
		metadata["refresh_interval_seconds"] = int64(interval)
	}
	return data
}

func refreshAfter(now, expires time.Time) time.Time {
	remaining := expires.Sub(now)
	if remaining <= 0 {
		return now
	}
	lead := 5 * time.Minute
	if remaining <= lead {
		lead = remaining / 2
	}
	return expires.Add(-lead).UTC()
}

func handleParseAuth(raw []byte) ([]byte, error) {
	var req authParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if !ownedAuth(req.FileName, req.Provider, req.RawJSON) {
		return okEnvelope(map[string]any{"handled": false}), nil
	}
	acct, err := authFromStorage(req.RawJSON)
	if err != nil {
		return okEnvelope(map[string]any{"handled": false}), nil
	}
	metadata, disabled := authMetadata(req.RawJSON)
	data := authData(acct, req.RawJSON, metadata, disabled)
	// CPA's watcher computes the identity from the file path. Never replace its ID.
	data.ID = ""
	data.FileName = req.FileName
	return okEnvelope(map[string]any{"handled": true, "auth": data}), nil
}

// refreshCodeartsToken now lives in errors.go with the retry helper.

func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req authRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newPluginError("invalid_request", http.StatusBadRequest, "invalid auth refresh request")
	}
	storage := []byte(req.StorageJSON)
	var file accountFile
	if req.AuthID != "" {
		var err error
		file, err = findAccountByID(req.AuthID)
		if err != nil {
			return nil, newPluginError("auth_not_found", http.StatusNotFound, "refresh: account not found")
		}
		unlock := lockAccount(file.Name)
		defer unlock()
		_, storage, err = accountStorage(file)
		if err != nil {
			return nil, newPluginError("auth_store_unavailable", http.StatusBadGateway, "refresh: could not read current account")
		}
	}
	acct, err := authFromStorage(storage)
	if err != nil {
		return nil, newPluginError("auth_invalid", http.StatusBadRequest, "refresh: invalid stored credentials")
	}
	originalRefreshToken := acct.RefreshToken
	if acct.RefreshToken == "" || acct.CodeVerifier == "" {
		return nil, newPluginError("auth_unusable", http.StatusForbidden, "refresh: missing refresh_token or code_verifier")
	}
	cfg := upstream.DefaultLoginConfig()
	if acct.ClientID != "" {
		cfg.ClientID = acct.ClientID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	token, err := refreshWithRetry(ctx, cfg, acct)
	if err != nil {
		status := http.StatusBadGateway
		if ctx.Err() != nil {
			status = http.StatusGatewayTimeout
		}
		if req.AuthID != "" && acct.RefreshToken != originalRefreshToken {
			// Re-read under the existing lock; never save an old STS snapshot or
			// extend its expiry just because the refresh token rotated.
			_, latest, readErr := accountStorage(file)
			if readErr != nil {
				return nil, newPluginError("refresh_save_failed", http.StatusBadGateway, "refresh failed: could not read account to save rotated refresh token")
			}
			updated, mergeErr := mergeAccountFields(latest, map[string]any{"refresh_token": acct.RefreshToken})
			if mergeErr != nil {
				return nil, newPluginError("refresh_save_failed", http.StatusBadGateway, "refresh failed: could not merge rotated refresh token")
			}
			if _, saveErr := hostAuthSave(map[string]any{"name": file.Name, "json": json.RawMessage(updated)}); saveErr != nil {
				return nil, newPluginError("refresh_save_failed", http.StatusBadGateway, "refresh failed: could not save rotated refresh token; sign in again if necessary")
			}
		}
		var expired *upstream.RefreshTokenExpiredError
		if errors.As(err, &expired) {
			return nil, newPluginError("refresh_token_expired", http.StatusUnauthorized, "refresh: refresh token expired or rejected; sign in again")
		}
		return nil, newPluginError("refresh_unavailable", status, "refresh: upstream token endpoint unavailable")
	}
	if token == nil {
		return nil, newPluginError("refresh_unavailable", http.StatusBadGateway, "refresh: missing token response")
	}
	credentials := token.Credentials
	if credentials.SecurityToken == "" {
		credentials.SecurityToken = token.Credential.SecurityToken
		credentials.AccessKeyID = token.Credential.Access
		credentials.SecretAccessKey = token.Credential.Secret
		credentials.Expiration = token.Credential.ExpiresAt
	}
	if credentials.SecurityToken == "" || credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" {
		return nil, newPluginError("refresh_incomplete", http.StatusBadGateway, "refresh: missing STS credentials")
	}
	// Merge into the original document rather than re-marshalling Auth: preserve
	// unknown fields, operator notes and auth metadata across token rotation.
	if req.AuthID != "" {
		_, storage, err = accountStorage(file)
		if err != nil {
			return nil, newPluginError("auth_store_unavailable", http.StatusBadGateway, "refresh: could not read current account for merge")
		}
	}
	changes := map[string]any{
		"cloud_dragon_token": credentials.SecurityToken,
		"access_key_id":      credentials.AccessKeyID,
		"secret_access_key":  credentials.SecretAccessKey,
		"updated_at":         time.Now().Unix(),
	}
	if credentials.Expiration != "" {
		changes["expiration"] = credentials.Expiration
	}
	if token.UserID != "" {
		changes["user_id"] = token.UserID
	}
	if token.UserName != "" {
		changes["user_name"] = token.UserName
	}
	if token.DomainID != "" {
		changes["domain_id"] = token.DomainID
	}
	// A prior failed attempt may have rotated even if the successful response
	// omits refresh_token.
	changes["refresh_token"] = acct.RefreshToken
	storage, err = mergeAccountFields(storage, changes)
	if err != nil {
		return nil, newPluginError("auth_invalid", http.StatusBadRequest, "refresh: could not merge credentials")
	}
	refreshed, err := authFromStorage(storage)
	if err != nil {
		return nil, err
	}
	metadata, disabled := authMetadata(storage)
	for key, value := range req.Metadata {
		if req.AuthID == "" {
			metadata[key] = value
		} else if _, exists := metadata[key]; !exists && key != "disabled" {
			metadata[key] = value
		}
	}
	if req.AuthID == "" {
		if flag, ok := metadata["disabled"].(bool); ok {
			disabled = flag
		}
	}
	data := authData(refreshed, storage, metadata, disabled)
	response := map[string]any{"auth": data}
	if !data.NextRefreshAfter.IsZero() {
		response["next_refresh_after"] = data.NextRefreshAfter
	}
	// Empty file_name and id make the host retain the current auth file and ID.
	return okEnvelope(response), nil
}

// hostAuthList/Get/Save are used by management and login without touching disk.
// They live in errors.go together with the error classification.
