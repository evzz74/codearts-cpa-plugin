package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"
)

func TestFailureDiagnosticsClassifyWithoutRawMessages(t *testing.T) {
	secret := "CANARY-private-token-and-prompt"
	cases := []struct {
		name       string
		err        error
		category   string
		httpStatus int
		code       string
	}{
		{"http400", &upstream.ApiError{Status: 400, HTTPStatus: 400, Message: secret}, "invalid_request", 400, ""},
		{"http401", &upstream.ApiError{Status: 401, HTTPStatus: 401, Message: secret}, "upstream_auth", 401, ""},
		{"http403", &upstream.ApiError{Status: 403, HTTPStatus: 403, Message: secret}, "upstream_forbidden", 403, ""},
		{"http404", &upstream.ApiError{Status: 404, HTTPStatus: 404, Message: secret}, "model_unavailable", 404, ""},
		{"http429", &upstream.ApiError{Status: 429, HTTPStatus: 429, Message: secret}, "rate_limited", 429, ""},
		{"http500", &upstream.ApiError{Status: 500, HTTPStatus: 500, Message: secret}, "upstream_unavailable", 500, ""},
		{"business200", &upstream.ApiError{Status: 400, HTTPStatus: 200, BusinessCode: "InferHub.002002009.404", Message: secret}, "model_unavailable", 200, "InferHub.002002009.404"},
		{"stream200", &upstream.UpstreamError{Code: "InferHub.4004.200", Msg: secret, HTTPStatus: 200}, "model_unavailable", 200, "InferHub.4004.200"},
		{"unknown200", &upstream.ApiError{Status: 429, HTTPStatus: 200, BusinessCode: secret, Message: "tpm not registered " + secret}, "unknown", 200, ""},
		{"unknownStatus", &upstream.ApiError{Status: 403, Message: secret}, "unknown", 0, ""},
		{"timeout", fmt.Errorf(secret+": %w", context.DeadlineExceeded), "timeout", 0, ""},
		{"canceled", context.Canceled, "canceled", 0, ""},
		{"network", &url.Error{Op: "Post", URL: "https://private.invalid/?token=" + secret, Err: errors.New(secret)}, "network", 0, ""},
		{"plugin", newPluginError("model_disabled", 403, secret), "model_disabled", 0, ""},
		{"unknown", errors.New(secret), "unknown", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf(secret+": %w", tc.err)
			d := failureDetails(wrapped)
			if d.Category != tc.category || d.HTTPStatus != tc.httpStatus || d.UpstreamCode != tc.code || d.Message == "" {
				t.Fatalf("unexpected diagnostic: %+v", d)
			}
			raw, _ := json.Marshal(d)
			safe := safeExecutionError(wrapped)
			for _, output := range []string{string(raw), safe.Error(), string(rpcErrorEnvelope(safe))} {
				if strings.Contains(output, secret) {
					t.Fatalf("secret escaped: %s", output)
				}
			}
			again := failureDetails(safe)
			if *again != *d {
				t.Fatalf("safe wrapping lost details: %+v vs %+v", again, d)
			}
		})
	}
}

func TestFailureDiagnosticsReachLedgerAndPublicResources(t *testing.T) {
	const secret = "CANARY-upstream-secret-and-user-content"
	for _, source := range []string{"http", "sse"} {
		for _, mode := range []string{"sync", "buffered", "async"} {
			t.Run(source+"/"+mode, func(t *testing.T) {
				fixture := setupModelHost(t)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if source == "http" {
						w.WriteHeader(403)
						_, _ = io.WriteString(w, `{"error":{"code":"`+secret+`","message":"`+secret+`"},"refresh_token":"`+secret+`"}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"error_code\":\"InferHub.002002009.404\",\"error_msg\":\""+secret+"\"}\n\n")
				}))
				defer server.Close()
				executorClient = func() *upstream.Client { return upstream.NewWithChatEndpoint(3*time.Second, server.URL) }
				req := executorRequest{AuthID: "id-1", Model: "GLM-5.3", StorageJSON: fixture.storage["idx-1"], Payload: []byte(`{"messages":[{"role":"user","content":"` + secret + `"}]}`), Metadata: map[string]any{"request_path": "/v1/messages"}}
				var transcript strings.Builder
				var callErr error
				if mode == "async" {
					req.StreamID = "diagnostic-stream"
					oldCall := executorHostCall
					t.Cleanup(func() { executorHostCall = oldCall })
					done := make(chan struct{})
					executorHostCall = func(method string, raw []byte) ([]byte, error) {
						var callback struct {
							Payload []byte `json:"payload"`
							Error   string `json:"error"`
						}
						_ = json.Unmarshal(raw, &callback)
						transcript.Write(callback.Payload)
						transcript.WriteString(callback.Error)
						if method == "host.stream.close" {
							close(done)
						}
						return okEnvelope(map[string]any{}), nil
					}
					raw, _ := json.Marshal(req)
					_, callErr = handleExecuteStream(raw)
					if callErr != nil {
						t.Fatal(callErr)
					}
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Fatal("async stream did not close")
					}
				} else {
					raw, _ := json.Marshal(req)
					if mode == "sync" {
						_, callErr = handleExecute(raw)
					} else {
						_, callErr = handleExecuteStream(raw)
					}
					if callErr == nil {
						t.Fatal("failure was accepted")
					}
					transcript.Write(rpcErrorEnvelope(callErr))
				}
				today, _, _, records, _ := ledger.snapshot("idx-1", 10, 62)
				if today.Requests != 1 || today.Failed != 1 || len(records) != 1 || records[0].Diagnostic == nil {
					t.Fatalf("failure not recorded once: %+v %+v", today, records)
				}
				d := records[0].Diagnostic
				if source == "http" && (d.HTTPStatus != 403 || d.Category != "upstream_forbidden" || d.UpstreamCode != "") {
					t.Fatalf("http details: %+v", d)
				}
				if source == "sse" && (d.HTTPStatus != 200 || d.UpstreamCode != "InferHub.002002009.404") {
					t.Fatalf("sse details: %+v", d)
				}
				raw, _ := json.Marshal(usageLedgerSnapshot("idx-1", 10))
				transcript.Write(raw)
				status, body := responseBody(t, managementReadProxy("usage", "idx-1"))
				if status != 200 {
					t.Fatalf("usage resource: %d", status)
				}
				raw, _ = json.Marshal(body)
				transcript.Write(raw)
				saved, err := os.ReadFile(filepath.Join(ledger.dir, ledgerFileName))
				if err != nil {
					t.Fatal(err)
				}
				transcript.Write(saved)
				if strings.Contains(transcript.String(), secret) {
					t.Fatal("raw upstream or request content leaked")
				}
			})
		}
	}
}

func TestModelTestPublishesOnlySafeDiagnostic(t *testing.T) {
	setupModelHost(t)
	status, body := responseBody(t, managementModelTest("idx-1", "unavailable"))
	if status != 200 || body["ok"] != false {
		t.Fatalf("model result: %d %v", status, body)
	}
	d, ok := body["diagnostic"].(map[string]any)
	if !ok || d["http_status"] != float64(403) || d["category"] != "upstream_forbidden" {
		t.Fatalf("missing diagnostic: %v", body)
	}
	status, body = responseBody(t, managementReadProxy("models", "idx-1"))
	if status != 200 {
		t.Fatal(status)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "upstream_forbidden") || strings.Contains(string(raw), "not entitled") {
		t.Fatalf("unsafe or missing cached diagnostic: %s", raw)
	}
}

func TestPrevalidationFailureRecordsOnceAndOldLedgerStillLoads(t *testing.T) {
	fixture := setupModelHost(t)
	if status, _ := responseBody(t, managementModelEnabled("idx-1", "GLM-5.3", false)); status != 200 {
		t.Fatal(status)
	}
	raw, _ := json.Marshal(executorRequest{AuthID: "id-1", Model: "GLM-5.3", StorageJSON: fixture.storage["idx-1"], StreamID: "unused", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	if _, err := handleExecuteStream(raw); errorCode(err) != "model_disabled" {
		t.Fatal(err)
	}
	today, _, _, records, _ := ledger.snapshot("idx-1", 10, 62)
	if today.Requests != 1 || len(records) != 1 || records[0].Diagnostic.Category != "model_disabled" {
		t.Fatalf("prevalidation accounting: %+v", today)
	}
	old := []byte(`{"version":1,"devices":{"old":{"records":[{"at":"2026-10-21T01:00:00Z","model":"GLM-5.3","failed":true,"total":0}]}}}`)
	path := filepath.Join(t.TempDir(), ledgerFileName)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	reloaded := &usageLedger{dir: filepath.Dir(path), devices: map[string]*usageLedgerAccount{}, now: time.Now}
	reloaded.loadLocked()
	_, _, _, records, _ = reloaded.snapshot("old", 10, 62)
	if len(records) != 1 || records[0].Diagnostic != nil {
		t.Fatal("old records were lost or invented diagnostics")
	}
}
