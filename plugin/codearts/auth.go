package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"

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

func authFromStorage(raw []byte) (*auth.Auth, error) {
	var acct auth.Auth
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

func authData(acct *auth.Auth, storage []byte, metadata map[string]any, disabled bool) pluginAuthData {
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["type"] = providerName
	label := acct.UserName
	if label == "" {
		label = acct.UserID
	}
	return pluginAuthData{Provider: providerName, Label: label, Disabled: disabled, StorageJSON: storage, Metadata: metadata}
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
	acct, err := authFromStorage(req.StorageJSON)
	if err != nil {
		return nil, authStorageError(fmt.Errorf("refresh: %w", err))
	}
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
		// A transient STS failure after a rotation should still persist the new
		// refresh token, so fall back to the rotated value where available.
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(req.StorageJSON, &fields); err != nil {
		return nil, err
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
	if token.RefreshToken != "" {
		changes["refresh_token"] = token.RefreshToken
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
	for key, value := range changes {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	storage, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	refreshed, err := authFromStorage(storage)
	if err != nil {
		return nil, err
	}
	metadata, disabled := authMetadata(req.StorageJSON)
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	if flag, ok := metadata["disabled"].(bool); ok {
		disabled = flag
	}
	data := authData(refreshed, storage, metadata, disabled)
	response := map[string]any{"auth": data}
	// The host prefers NextRefreshAfter over parsing expired credentials, so
	// report the STS expiry to keep refresh timing on time.
	if expires := refreshed.ExpiresAt(); !expires.IsZero() {
		response["next_refresh_after"] = expires.UTC()
	}
	// Empty file_name and id make the host retain the current auth file and ID.
	return okEnvelope(response), nil
}

// hostAuthList/Get/Save are used by management and login without touching disk.
// They live in errors.go together with the error classification.
