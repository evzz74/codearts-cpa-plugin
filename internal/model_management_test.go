package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type modelHostFixture struct {
	mu      sync.Mutex
	files   []accountFile
	storage map[string][]byte
}

func setupModelHost(t *testing.T) *modelHostFixture {
	t.Helper()
	oldList, oldGet, oldSave, oldDiscover, oldClient, oldLedger := hostAuthList, hostAuthGet, hostAuthSave, discoverModels, executorClient, ledger
	fixture := &modelHostFixture{files: []accountFile{{ID: "id-1", AuthIndex: "idx-1", Name: "codearts-one.json"}, {ID: "id-2", AuthIndex: "idx-2", Name: "codearts-two.json"}}, storage: map[string][]byte{}}
	for _, f := range fixture.files {
		fixture.storage[f.AuthIndex] = []byte(fmt.Sprintf(`{"type":"codearts","user_id":%q,"user_name":%q,"cloud_dragon_token":"test-token","access_key_id":"test-ak","secret_access_key":"test-sk","custom_field":{"keep":true}}`, f.ID, f.ID))
	}
	hostAuthList = func() (json.RawMessage, error) { return json.Marshal(map[string]any{"files": fixture.files}) }
	hostAuthGet = func(index string) (json.RawMessage, error) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		storage, ok := fixture.storage[index]
		if !ok {
			return nil, fmt.Errorf("missing account")
		}
		return json.Marshal(map[string]any{"json": json.RawMessage(storage)})
	}
	hostAuthSave = func(value any) (json.RawMessage, error) {
		raw, _ := json.Marshal(value)
		var save struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		}
		if err := json.Unmarshal(raw, &save); err != nil {
			return nil, err
		}
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		for _, f := range fixture.files {
			if f.Name == save.Name {
				fixture.storage[f.AuthIndex] = append([]byte(nil), save.JSON...)
				return json.RawMessage(`{}`), nil
			}
		}
		return nil, fmt.Errorf("missing account")
	}
	discoverModels = func(*Auth) ([]upstream.ModelInfo, error) {
		return []upstream.ModelInfo{{ID: "GLM-5.2", Name: "GLM"}, {ID: "unavailable", Name: "Unavailable"}}, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model == "unavailable" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"not entitled"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")
	}))
	executorClient = func() *upstream.Client { return upstream.NewWithChatEndpoint(3*time.Second, server.URL) }
	ledger = newTestLedger(t)
	ledger.now = time.Now
	modelTests.Lock()
	oldTests := modelTests.accounts
	modelTests.accounts = map[string]*modelTestState{}
	modelTests.Unlock()
	t.Cleanup(func() {
		server.Close()
		hostAuthList, hostAuthGet, hostAuthSave, discoverModels, executorClient, ledger = oldList, oldGet, oldSave, oldDiscover, oldClient, oldLedger
		modelTests.Lock()
		modelTests.accounts = oldTests
		modelTests.Unlock()
	})
	return fixture
}

func TestRefreshMetadataCannotOverwriteStorage(t *testing.T) {
	fixture := setupModelHost(t)
	fixture.storage["idx-1"], _ = mergeAccountFields(fixture.storage["idx-1"], map[string]any{"refresh_token": "old-refresh", "code_verifier": "pkce", "disabled_models": []string{"GLM-5.2"}})
	oldRefresh := refreshCodeartsToken
	t.Cleanup(func() { refreshCodeartsToken = oldRefresh })
	refreshCodeartsToken = func(context.Context, upstream.LoginConfig, *Auth) (*upstream.TokenResponse, error) {
		return &upstream.TokenResponse{RefreshToken: "new-refresh", Credentials: upstream.Credentials{SecurityToken: "new-sts", AccessKeyID: "new-ak", SecretAccessKey: "new-sk", Expiration: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}, nil
	}
	request, _ := json.Marshal(authRefreshRequest{AuthID: "id-1", StorageJSON: fixture.storage["idx-1"], Metadata: map[string]any{"cloud_dragon_token": "old-sts", "refresh_token": "old-refresh", "access_key_id": "old-ak", "disabled_models": []string{}}})
	response, err := handleRefreshAuth(request)
	if err != nil {
		t.Fatal(err)
	}
	var data pluginAuthData
	_ = json.Unmarshal(envelopeResult(t, response)["auth"], &data)
	var merged map[string]any
	_ = json.Unmarshal(data.StorageJSON, &merged)
	for key, value := range data.Metadata {
		merged[key] = value
	}
	if merged["cloud_dragon_token"] != "new-sts" || merged["refresh_token"] != "new-refresh" || merged["access_key_id"] != "new-ak" || len(merged["disabled_models"].([]any)) != 1 {
		t.Fatal("host metadata overlay replaced refreshed credentials or policy")
	}
	if _, err := saveRefreshedAccount(fixture.files[0], data); err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	_ = json.Unmarshal(fixture.storage["idx-1"], &saved)
	if saved["expired"] != data.Metadata["expired"] || saved["refresh_interval_seconds"] == nil || saved["cloud_dragon_token"] != "new-sts" || len(saved["disabled_models"].([]any)) != 1 {
		t.Fatal("keepalive did not preserve policy and host schedule")
	}
}

func TestManagedModelsPersistAndBlockAllExecutionPaths(t *testing.T) {
	fixture := setupModelHost(t)
	oldAliases := modelAliases.byName
	modelAliases.byName = map[string]string{"my-glm": "GLM-5.2"}
	t.Cleanup(func() { modelAliases.byName = oldAliases })
	stale := append([]byte(nil), fixture.storage["idx-1"]...)
	status, _ := responseBody(t, managementModelEnabled("idx-1", "my-glm", false))
	if status != 200 {
		t.Fatalf("disable: %d", status)
	}
	if !modelDisabled(fixture.storage["idx-1"], "codearts/GLM-5.2") || modelDisabled(fixture.storage["idx-2"], "GLM-5.2") || !strings.Contains(string(fixture.storage["idx-1"]), `"custom_field":{"keep":true}`) {
		t.Fatal("policy not isolated or storage lost")
	}
	for _, model := range []string{"GLM-5.2", "codearts/GLM-5.2", "my-glm", "codearts/my-glm"} {
		for _, streamID := range []string{"", "async-test"} {
			raw, _ := json.Marshal(executorRequest{AuthID: "id-1", Model: model, StorageJSON: stale, Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), StreamID: streamID})
			if _, err := handleExecute(raw); errorCode(err) != "model_disabled" {
				t.Fatalf("sync %s: %v", model, err)
			}
			if _, err := handleExecuteStream(raw); errorCode(err) != "model_disabled" {
				t.Fatalf("stream %s: %v", model, err)
			}
		}
	}
	raw, _ := json.Marshal(pluginapi.AuthModelRequest{AuthID: "id-1", StorageJSON: stale})
	response, err := handleModelForAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	var models []pluginModelInfo
	_ = json.Unmarshal(envelopeResult(t, response)["models"], &models)
	if len(models) != 1 || models[0].ID != "unavailable" {
		t.Fatalf("discovery filter: %+v", models)
	}
	status, body := responseBody(t, managementModels("idx-1"))
	if status != 200 || len(body["models"].([]any)) != 2 {
		t.Fatalf("disabled model cannot be re-enabled: %v", body)
	}
	status, _ = responseBody(t, managementModelEnabled("idx-1", "GLM-5.2", true))
	if status != 200 || modelDisabled(fixture.storage["idx-1"], "GLM-5.2") {
		t.Fatal("enable failed")
	}
}

func TestManagedModelProbeRecordsSuccessAndFailure(t *testing.T) {
	setupModelHost(t)
	status, body := responseBody(t, managementModelTest("idx-1", "GLM-5.2"))
	if status != 200 || body["ok"] != true {
		t.Fatalf("success probe: %d %v", status, body)
	}
	today, _, _, records, _ := ledger.snapshot("idx-1", 10, 62)
	if today.Requests != 1 || today.Failed != 0 || len(records) != 1 || records[0].Output <= 0 || !records[0].Estimated {
		t.Fatalf("probe usage: %+v %+v", today, records)
	}
	status, _ = responseBody(t, managementModelTest("idx-1", "GLM-5.2"))
	if status != 429 {
		t.Fatalf("cooldown bypassed: %d", status)
	}
	status, body = responseBody(t, managementModelTest("idx-2", "unavailable"))
	if status != 200 || body["ok"] != false {
		t.Fatalf("failure probe: %d %v", status, body)
	}
	today, _, _, records, _ = ledger.snapshot("idx-2", 10, 62)
	if today.Requests != 1 || today.Failed != 1 || len(records) != 1 {
		t.Fatalf("failure usage: %+v", today)
	}
	status, body = responseBody(t, managementModels("idx-1"))
	row := body["models"].([]any)[0].(map[string]any)
	if status != 200 || row["test"].(map[string]any)["ok"] != true {
		t.Fatalf("test result lost: %v", body)
	}
}

func TestExecutorStreamsRecordSuccessfulUsage(t *testing.T) {
	fixture := setupModelHost(t)
	oldCall := executorHostCall
	t.Cleanup(func() { executorHostCall = oldCall })
	closed := make(chan struct{}, 1)
	executorHostCall = func(method string, _ []byte) ([]byte, error) {
		if method == "host.stream.close" {
			closed <- struct{}{}
		}
		return okEnvelope(map[string]any{}), nil
	}
	req := executorRequest{AuthID: "id-1", Model: "GLM-5.2", StorageJSON: fixture.storage["idx-1"], Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`), Metadata: map[string]any{"request_path": "/v1/chat/completions"}}
	for _, id := range []string{"", "test-stream"} {
		req.StreamID = id
		raw, _ := json.Marshal(req)
		if _, err := handleExecuteStream(raw); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("async stream did not finish")
	}
	today, _, _, records, _ := ledger.snapshot("idx-1", 10, 62)
	if today.Requests != 2 || today.Failed != 0 || len(records) != 2 {
		t.Fatalf("stream usage lost: %+v", today)
	}
	for _, record := range records {
		if !record.Stream || !record.Estimated || record.Input <= 0 || record.Output <= 0 {
			t.Fatalf("incomplete usage: %+v", record)
		}
	}
}

func TestModelManagementRequiresAuthenticatedPost(t *testing.T) {
	setupModelHost(t)
	managementSettings.Lock()
	oldKey := managementSettings.key
	managementSettings.key = "model-test-key"
	managementSettings.Unlock()
	t.Cleanup(func() { managementSettings.Lock(); managementSettings.key = oldKey; managementSettings.Unlock() })
	for _, route := range []string{"/models/test", "/models/enabled"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			r := managementRequest{Method: method, Path: "/v0/management/plugins/codearts" + route, Body: []byte(`{"auth_index":"idx-1","model":"GLM-5.2","enabled":false}`)}
			if method == http.MethodGet {
				r.Headers = http.Header{"Authorization": []string{"Bearer model-test-key"}}
			}
			raw, _ := json.Marshal(r)
			response, err := managementHandle(raw)
			if err != nil {
				t.Fatal(err)
			}
			status, _ := responseBody(t, response)
			if method == http.MethodPost && status != 401 || method == http.MethodGet && status != 404 {
				t.Fatalf("%s %s: %d", method, route, status)
			}
		}
		status, _ := responseBody(t, managementReadProxy(strings.TrimPrefix(route, "/"), "idx-1"))
		if status != 400 {
			t.Fatalf("resource mutation exposed: %d", status)
		}
	}
}

func TestExecutorUsageIgnoresDeviceAndMigratesLegacyOnce(t *testing.T) {
	fixture := setupModelHost(t)
	req := executorRequest{AuthID: "id-1", Model: "GLM-5.2", StorageJSON: fixture.storage["idx-1"], Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}
	for _, device := range []string{"device-a", "device-b"} {
		req.Metadata = map[string]any{"device_id": device}
		raw, _ := json.Marshal(req)
		if _, err := handleExecute(raw); err != nil {
			t.Fatal(err)
		}
	}
	ledger.record(usageEntry{Device: "id-1", Total: 7})
	acct, _, _ := accountStorage(fixture.files[0])
	migrateLegacyUsage(fixture.files[0], acct)
	migrateLegacyUsage(fixture.files[0], acct)
	today, _, _, records, _ := ledger.snapshot("idx-1", 10, 62)
	if today.Requests != 3 || len(records) != 3 || today.Total <= 7 {
		t.Fatalf("wrong account ledger: %+v", today)
	}
	if _, _, _, records, _ := ledger.snapshot("id-1", 10, 62); len(records) != 0 {
		t.Fatal("legacy source not migrated")
	}
	reloaded := &usageLedger{dir: ledger.dir, devices: map[string]*usageLedgerAccount{}, now: time.Now}
	reloaded.loadLocked()
	if today, _, _, _, _ := reloaded.snapshot("idx-1", 10, 62); today.Requests != 3 {
		t.Fatal("migration not persisted")
	}
}
