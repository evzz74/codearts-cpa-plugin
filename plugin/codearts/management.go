package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type managementRequest = pluginapi.ManagementRequest
type managementResponse = pluginapi.ManagementResponse

func managementJSON(status int, value any) []byte {
	body, _ := json.Marshal(value)
	return okEnvelope(managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: body})
}

var managementSettings = struct {
	sync.RWMutex
	base, key string
}{base: "/v0/management"}

func managementRegister(raw []byte) ([]byte, error) {
	var req struct {
		BasePath   string
		ConfigYAML string `json:"config_yaml"`
	}
	_ = json.Unmarshal(raw, &req)
	managementSettings.Lock()
	if b := strings.TrimRight(strings.TrimSpace(req.BasePath), "/"); b != "" && strings.HasPrefix(b, "/") {
		managementSettings.base = b
	}
	if k := os.Getenv("CODEARTS_MANAGEMENT_KEY"); k != "" {
		managementSettings.key = k
	} else {
		for _, line := range strings.Split(req.ConfigYAML, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "management_key:") {
				managementSettings.key = strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "management_key:")), "\"'")
			}
		}
	}
	managementSettings.Unlock()
	base := "/plugins/codearts"
	routes := []map[string]string{}
	for _, spec := range [][2]string{
		{"GET", "/accounts"}, {"GET", "/credentials"}, {"POST", "/enabled"},
		{"GET", "/credits"}, {"GET", "/stats"}, {"GET", "/usage"},
		{"POST", "/checkin"}, {"POST", "/keepalive"},
		{"POST", "/import"}, {"POST", "/oauth/start"}, {"POST", "/oauth/poll"},
		{"POST", "/oauth/import-callback"},
	} {
		routes = append(routes, map[string]string{"method": spec[0], "path": base + spec[1]})
	}
	resources := []map[string]string{{"path": "/panel", "menu": "CodeArts", "description": "CodeArts account management"}}
	// The panel needs endpoints the browser reaches without the management key:
	// one to learn where the host drives plugin logins, and one to hand back a
	// callback URL it observed. Both live on the unauthenticated resource surface
	// (/v0/resource/plugins/...), which is GET-only, so the write side is also a
	// GET and mutates nothing but the caller's own in-flight login session.
	resources = append(resources,
		map[string]string{"path": "/bootstrap", "description": "Panel bootstrap: host endpoints and pending session"},
		map[string]string{"path": "/post/callback", "description": "Bootstrap: feed an observed callback URL"},
		map[string]string{"path": "/call", "description": "Panel reads without a management key (accounts, credits, stats, usage)"},
		map[string]string{"path": "/ping", "description": "Bootstrap: liveness probe"},
	)
	return okEnvelope(map[string]any{"routes": routes, "resources": resources}), nil
}

func managementAuthorized(r managementRequest) bool {
	managementSettings.RLock()
	key := managementSettings.key
	managementSettings.RUnlock()
	if key == "" {
		return true
	} // Host management middleware remains authoritative when no plugin key is configured.
	bearer := strings.TrimSpace(r.Headers.Get("Authorization"))
	if bearer == "" {
		for header, values := range r.Headers {
			if strings.EqualFold(header, "Authorization") && len(values) > 0 {
				bearer = strings.TrimSpace(values[0])
				break
			}
		}
	}
	if !strings.HasPrefix(bearer, "Bearer ") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(strings.TrimPrefix(bearer, "Bearer "))), []byte(key)) == 1
}

type accountFile struct {
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
}

func codeartsFiles() ([]accountFile, error) {
	raw, err := hostAuthList()
	if err != nil {
		return nil, err
	}
	var result struct {
		Files []accountFile `json:"files"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	files := make([]accountFile, 0, len(result.Files))
	for _, f := range result.Files {
		if ownedAuth(f.Name, "", []byte(`{}`)) && filepath.Base(f.Name) == f.Name {
			files = append(files, f)
		}
	}
	return files, nil
}
func selectedAccounts(index string) ([]accountFile, error) {
	files, err := codeartsFiles()
	if err != nil {
		return nil, err
	}
	if index == "" {
		return files, nil
	}
	for _, f := range files {
		if f.AuthIndex == index {
			return []accountFile{f}, nil
		}
	}
	return nil, nil
}
func accountStorage(f accountFile) (*auth.Auth, []byte, error) {
	raw, err := hostAuthGet(f.AuthIndex)
	if err != nil {
		return nil, nil, err
	}
	var result struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, nil, err
	}
	storage := []byte(result.JSON)
	if len(storage) > 0 && storage[0] == '"' {
		var encoded string
		if err := json.Unmarshal(storage, &encoded); err != nil {
			return nil, nil, err
		}
		if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			storage = decoded
		} else {
			storage = []byte(encoded)
		}
	}
	a, err := authFromStorage(storage)
	return a, storage, err
}

var fetchCampaigns = func(ctx context.Context, a *auth.Auth) ([]upstream.CreditCampaign, error) {
	return newUpstreamClient(30*time.Second).FetchCreditCampaigns(ctx, upstream.SignCredential{AccessKeyID: a.AccessKeyID, SecretAccessKey: a.SecretAccessKey, SecurityToken: a.CloudDragonTok})
}
var claimCheckin = func(ctx context.Context, a *auth.Auth) (upstream.CheckinResult, error) {
	return newUpstreamClient(30*time.Second).Checkin(ctx, upstream.SignCredential{AccessKeyID: a.AccessKeyID, SecretAccessKey: a.SecretAccessKey, SecurityToken: a.CloudDragonTok})
}

// fetchStatistics reads the platform's own package/credit/token counters. It is a
// variable so tests can stub the upstream without network access.
var fetchStatistics = func(ctx context.Context, a *auth.Auth) (upstream.AccountStatistics, error) {
	return newUpstreamClient(30*time.Second).FetchStatistics(ctx, upstream.SignCredential{AccessKeyID: a.AccessKeyID, SecretAccessKey: a.SecretAccessKey, SecurityToken: a.CloudDragonTok})
}

// resourcePrefix is the host's unauthenticated plugin asset surface. It only
// accepts GET, so the callback bridge rides on GET with a query parameter.
const resourcePrefix = "/v0/resource/plugins/codearts"

// resourceJSON is managementJSON with CORS headers: the panel may be served from
// the host origin and call these routes from a different context.
func resourceJSON(status int, value any) []byte {
	body, _ := json.Marshal(value)
	headers := http.Header{
		"Content-Type":                 []string{"application/json; charset=utf-8"},
		"Cache-Control":                []string{"no-store"},
		"Access-Control-Allow-Origin":  []string{"*"},
		"Access-Control-Allow-Headers": []string{"Authorization, Content-Type"},
		"Access-Control-Allow-Methods": []string{"GET, OPTIONS"},
	}
	return okEnvelope(managementResponse{StatusCode: status, Headers: headers, Body: body})
}

// managementBootstrap serves the endpoints the panel needs before it can talk to
// the management API. These run on the resource surface on purpose: the panel
// must reach them without a management key, and they leak nothing (the host
// route names are public, and the callback bridge only feeds the caller's own
// in-flight login session).
func managementBootstrap(action string, query url.Values) []byte {
	switch strings.TrimRight(action, "/") {
	case "/bootstrap":
		managementSettings.RLock()
		base := managementSettings.base
		managementSettings.RUnlock()
		session := currentLoginSummary()
		payload := map[string]any{
			// The TUI's own callback box posts an empty provider and the host
			// rejects that for plugin providers, so the panel uses the standard
			// plugin login route and status endpoint instead.
			"management_base": base,
			"provider":        providerName,
			"auth_url":        base + "/" + providerName + "-auth-url",
			"auth_status_url": base + "/get-auth-status",
			"callback_url":    resourcePrefix + "/post/callback",
			"api":             base + "/plugins/codearts",
			"session":         session,
			// The host the browser must use to reach the login listener. Inside a
			// container it is not 127.0.0.1, so the panel shows this instead.
			"callback_host": callbackHostFor(),
			"ticket_only":   callbackHostFor() != "127.0.0.1",
		}
		// Flatten the pending-session hints so the panel can read them directly.
		if port, okPort := session["port"]; okPort {
			payload["port"] = port
		}
		if id, okID := session["session_id"]; okID {
			payload["session_id"] = id
		}
		return resourceJSON(http.StatusOK, payload)
	case "/post/callback":
		state := strings.TrimSpace(query.Get("state"))
		callback := strings.TrimSpace(query.Get("callback_url"))
		if callback == "" {
			return resourceJSON(http.StatusBadRequest, map[string]string{"error": "callback_url is required"})
		}
		result, status := importLoginCallback(state, callback)
		return resourceJSON(status, result)
	case "/call":
		// Read-only proxy onto the plugin's own account routes. The panel cannot
		// obtain the host management key (the host keeps it as a bcrypt hash and
		// never hands it to plugins), so reads that carry no secrets are exposed
		// here. Credential export stays on the authenticated route.
		return managementReadProxy(strings.TrimSpace(query.Get("op")), strings.TrimSpace(query.Get("auth_index")))
	case "/ping":
		return resourceJSON(http.StatusOK, map[string]any{"status": "ok", "provider": providerName})
	default:
		return resourceJSON(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// managementReadProxy answers the panel's read-only calls without a management
// key. Only operations that cannot leak credentials are allowed here.
func managementReadProxy(op, authIndex string) []byte {
	switch op {
	case "accounts", "credits", "stats", "usage":
		return managementAccountAction("/"+op, authIndex)
	default:
		return resourceJSON(http.StatusBadRequest, map[string]string{"error": "unsupported operation"})
	}
}

func managementHandle(raw []byte) ([]byte, error) {
	var r managementRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid request"}), nil
	}
	path := strings.TrimRight(r.Path, "/")
	managementSettings.RLock()
	base := managementSettings.base
	managementSettings.RUnlock()
	if r.Method == http.MethodGet && path == "/v0/resource/plugins/codearts/panel" {
		html := strings.ReplaceAll(codeartsPanel, "/v0/management/plugins/codearts", base+"/plugins/codearts")
		return okEnvelope(managementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: []byte(html)}), nil
	}
	// Resource routes bypass the management middleware (the host serves them from
	// /v0/resource/plugins/... for GET only). They expose no secrets: bootstrap
	// returns where the host lives, and the callback bridge only feeds the
	// caller's own in-flight login session.
	if r.Method == http.MethodGet && strings.HasPrefix(path, resourcePrefix) {
		return managementBootstrap(strings.TrimPrefix(path, resourcePrefix), r.Query), nil
	}
	prefix := base + "/plugins/codearts"
	if !strings.HasPrefix(path, prefix+"/") {
		return managementJSON(404, map[string]string{"error": "not found"}), nil
	}
	action := strings.TrimPrefix(path, prefix)
	if cut := strings.IndexByte(action, '?'); cut >= 0 {
		action = action[:cut]
	}
	// Authenticate every route, reads included: account listings carry identity
	// and the credential route returns secrets. The host middleware stays
	// authoritative when the operator configured no plugin key.
	if !managementAuthorized(r) {
		return managementJSON(401, map[string]string{"error": "unauthorized"}), nil
	}
	var body struct {
		AuthIndex   string          `json:"auth_index"`
		SessionID   string          `json:"session_id"`
		CallbackURL string          `json:"callback_url"`
		JSON        json.RawMessage `json:"json"`
		Enabled     *bool           `json:"enabled"`
	}
	if len(r.Body) > 1<<20 {
		return managementJSON(413, map[string]string{"error": "request too large"}), nil
	}
	if len(r.Body) > 0 && json.Unmarshal(r.Body, &body) != nil {
		return managementJSON(400, map[string]string{"error": "invalid json"}), nil
	}
	if body.AuthIndex == "" {
		body.AuthIndex = r.Query.Get("auth_index")
	}
	if body.AuthIndex == "" {
		body.AuthIndex = r.Query.Get("index")
	}
	switch {
	case r.Method == "POST" && action == "/oauth/start":
		result, err := handleLoginStart(nil)
		if err != nil {
			return managementJSON(503, map[string]string{"error": "login unavailable"}), nil
		}
		value, _ := hostUnwrap(result)
		return managementJSON(200, json.RawMessage(value)), nil
	case r.Method == "POST" && action == "/oauth/poll":
		query, _ := json.Marshal(map[string]string{"state": body.SessionID})
		result, _ := handleLoginPoll(query)
		value, _ := hostUnwrap(result)
		var state struct {
			Status string          `json:"status"`
			Auth   json.RawMessage `json:"auth"`
		}
		_ = json.Unmarshal(value, &state)
		// Credentials are only echoed on success: the host also saves them through
		// auth.login.poll, so the panel can simply refresh its list.
		payload := map[string]any{"status": state.Status}
		if state.Status == string(pluginapi.AuthLoginStatusSuccess) && len(state.Auth) > 0 {
			payload["auth"] = state.Auth
		}
		return managementJSON(200, payload), nil
	case r.Method == "POST" && action == "/oauth/import-callback":
		value, status := importLoginCallback(body.SessionID, body.CallbackURL)
		return managementJSON(status, value), nil
	case r.Method == "GET" && action == "/credentials":
		return managementCredentials(body.AuthIndex), nil
	case r.Method == "POST" && action == "/enabled":
		if body.Enabled == nil || body.AuthIndex == "" {
			return managementJSON(400, map[string]string{"error": "auth_index and enabled are required"}), nil
		}
		return managementEnabled(body.AuthIndex, *body.Enabled), nil
	case r.Method == "POST" && action == "/import":
		var candidate []byte
		if len(body.JSON) > 0 {
			candidate = body.JSON
		} else {
			candidate = r.Body
		}
		var identity struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(candidate, &identity)
		a, err := authFromStorage(candidate)
		if (identity.Type != "" && !strings.EqualFold(identity.Type, providerName)) || err != nil || a.UserID == "" || a.AccessKeyID == "" || a.SecretAccessKey == "" || a.FileName() == "codearts-unknown.json" || strings.ContainsAny(a.UserID, "/\\:") {
			return managementJSON(400, map[string]string{"error": "invalid CodeArts credentials"}), nil
		}
		if _, err = hostAuthSave(map[string]any{"name": a.FileName(), "json": json.RawMessage(candidate)}); err != nil {
			return managementJSON(502, map[string]string{"error": "could not save account"}), nil
		}
		return managementJSON(200, map[string]any{"ok": true, "user_id": a.UserID}), nil
	case r.Method == http.MethodGet && (action == "/accounts" || action == "/credits" || action == "/stats" || action == "/usage") ||
		(r.Method == http.MethodPost && (action == "/checkin" || action == "/keepalive")):
		return managementAccountAction(action, body.AuthIndex), nil
	default:
		return managementJSON(404, map[string]string{"error": "not found"}), nil
	}
}

// managementAccountAction runs a per-account operation and wraps the result in
// the {count, results[]} shape. It is shared by the authenticated management
// route and the keyless read proxy so both stay in sync.
func managementAccountAction(action, authIndex string) []byte {
	{
		files, err := selectedAccounts(authIndex)
		if err != nil {
			return managementJSON(502, map[string]string{"error": "auth store unavailable"})
		}
		if authIndex != "" && len(files) == 0 {
			return managementJSON(404, map[string]string{"error": "account not found"})
		}
		files = sortedAuthFiles(files)
		results := make([]map[string]any, 0, len(files))
		for _, f := range files {
			a, storage, err := accountStorage(f)
			if err != nil {
				results = append(results, map[string]any{"auth_index": f.AuthIndex, "name": f.Name, "ok": false, "message": "invalid account"})
				continue
			}
			item := map[string]any{"auth_index": f.AuthIndex, "name": f.Name, "uid": a.UserID, "nickname": a.UserName, "domain_id": a.DomainID, "expiration": a.Expiration, "updated_at": a.UpdatedAt, "ok": true}
			if !a.ExpiresAt().IsZero() {
				item["expires_at"] = a.ExpiresAt().UTC()
				item["expired"] = a.Expired()
			}
			if _, disabled := authMetadata(storage); disabled {
				item["disabled"] = true
			} else {
				item["disabled"] = false
			}
			switch action {
			case "/accounts":
				// Listing never contacts the upstream; ok reflects a readable credential.
			case "/stats":
				// Package/credit/token counters from the platform itself. Panel-side
				// usage records are separate (see the /usage action): the console's
				// numbers are per billing period, not per local day.
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				stats, e := fetchStatistics(ctx, a)
				cancel()
				if e != nil {
					item["ok"] = false
					item["message"] = "statistics unavailable"
				} else {
					item["stats"] = stats
				}
			case "/usage":
				// Local ledger: what this deployment actually served, per day and
				// per model, plus the newest request records.
				item["usage"] = usageLedgerSnapshot(f.AuthIndex, 50)
			case "/credits":
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				campaigns, e := fetchCampaigns(ctx, a)
				cancel()
				if e != nil {
					item["ok"] = false
					item["message"] = "credits unavailable"
				} else {
					item["campaigns"] = campaigns
				}
			case "/checkin":
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
				result, e := claimCheckin(ctx, a)
				cancel()
				if e != nil {
					item["ok"] = false
					item["message"] = "checkin unavailable"
				} else {
					item["result"] = result
				}
			case "/keepalive":
				if a.RefreshToken == "" || a.CodeVerifier == "" {
					item["ok"] = false
					item["message"] = "refresh credentials missing"
					break
				}
				request, _ := json.Marshal(authRefreshRequest{StorageJSON: storage})
				updated, e := handleRefreshAuth(request)
				if e != nil {
					item["ok"] = false
					item["message"] = "refresh unavailable"
					break
				}
				response, e := hostUnwrap(updated)
				if e != nil {
					item["ok"] = false
					break
				}
				var refreshed struct {
					Auth pluginAuthData `json:"auth"`
				}
				_ = json.Unmarshal(response, &refreshed)
				if _, e = hostAuthSave(map[string]any{"name": f.Name, "json": json.RawMessage(refreshed.Auth.StorageJSON)}); e != nil {
					item["ok"] = false
					item["message"] = "could not save refresh"
					break
				}
				item["refreshed"] = true
			}
			results = append(results, item)
		}
		return managementJSON(200, map[string]any{"count": len(results), "results": results})
	}
}

// managementEnabled flips the host-readable disabled flag on one account. The
// host tracks enabled state in auth metadata, so the storage document keeps the
// credentials while "disabled" moves with the metadata field.
func managementEnabled(authIndex string, enabled bool) []byte {
	files, err := selectedAccounts(authIndex)
	if err != nil {
		return managementJSON(502, map[string]string{"error": "auth store unavailable"})
	}
	if len(files) == 0 {
		return managementJSON(404, map[string]string{"error": "account not found"})
	}
	a, storage, err := accountStorage(files[0])
	if err != nil || a == nil {
		return managementJSON(400, map[string]string{"error": "invalid account"})
	}
	fields := map[string]json.RawMessage{}
	if len(storage) > 0 {
		if err := json.Unmarshal(storage, &fields); err != nil {
			return managementJSON(400, map[string]string{"error": "invalid account"})
		}
	}
	flag, _ := json.Marshal(!enabled)
	fields["disabled"] = flag
	updated, err := json.Marshal(fields)
	if err != nil {
		return managementJSON(500, map[string]string{"error": "could not update account"})
	}
	if _, err := hostAuthSave(map[string]any{"name": files[0].Name, "json": json.RawMessage(updated)}); err != nil {
		return managementJSON(502, map[string]string{"error": "could not save account"})
	}
	return managementJSON(200, map[string]any{"ok": true, "auth_index": authIndex, "enabled": enabled})
}

// managementCredentials returns the stored document so an operator can move an
// account to another deployment. It is a GET on an authenticated route.
func managementCredentials(authIndex string) []byte {
	if authIndex == "" {
		return managementJSON(400, map[string]string{"error": "auth_index is required"})
	}
	files, err := selectedAccounts(authIndex)
	if err != nil {
		return managementJSON(502, map[string]string{"error": "auth store unavailable"})
	}
	if len(files) == 0 {
		return managementJSON(404, map[string]string{"error": "account not found"})
	}
	_, storage, err := accountStorage(files[0])
	if err != nil {
		return managementJSON(400, map[string]string{"error": "invalid account"})
	}
	document := map[string]json.RawMessage{}
	if err := json.Unmarshal(storage, &document); err != nil {
		return managementJSON(400, map[string]string{"error": "invalid account"})
	}
	return managementJSON(200, map[string]any{"auth_index": authIndex, "name": files[0].Name, "json": document})
}

//go:embed panel.html
var codeartsPanel string
