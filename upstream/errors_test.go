package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPErrorDiagnostics(t *testing.T) {
	bodies := []struct {
		name, body, code string
	}{
		{"flat", `{"error_code":"InferHub.denied","error_msg":"internal detail"}`, "InferHub.denied"},
		{"nested", `{"error":{"code":"model_not_found","message":"internal detail"}}`, "model_not_found"},
		{"flat numeric", `{"error_code":9007199254740993,"error_msg":"internal detail"}`, "9007199254740993"},
		{"nested numeric", `{"error":{"code":9007199254740993,"message":"internal detail"}}`, "9007199254740993"},
		{"HTML", `<html>internal detail</html>`, ""},
		{"malformed", `{"error_code":"not complete"`, ""},
		{"object code", `{"error":{"code":{"secret":"internal detail"},"message":"failure"}}`, ""},
		{"boolean code", `{"error_code":true,"error_msg":"failure"}`, ""},
		{"null code", `{"error_code":null,"error_msg":"failure"}`, ""},
	}
	for _, status := range []int{400, 401, 403, 404, 429, 500, 502, 503} {
		for _, body := range bodies {
			for _, path := range []string{"sendChatV2", "ChatStream", "doJSON"} {
				t.Run(fmt.Sprintf("%s/%d/%s", path, status, body.name), func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(status)
						_, _ = io.WriteString(w, body.body)
					}))
					defer srv.Close()
					c := NewWithChatEndpoint(time.Second, srv.URL)
					var err error
					switch path {
					case "sendChatV2":
						_, err = c.SendChatV2(context.Background(), map[string]any{"model": "GLM-5.3"}, "trace", SignCredential{}, "token", false)
					case "ChatStream":
						_, err = c.ChatStream(context.Background(), "chat", []ChatMessage{{Role: "user", Content: "hi"}}, "trace", SignCredential{}, "", "GLM-5.2", false)
					case "doJSON":
						err = c.doJSON(context.Background(), http.MethodPost, srv.URL, EpChatV2, nil, nil, nil)
					}
					if err == nil {
						t.Fatal("error response accepted")
					}
					var apiErr *ApiError
					if !errors.As(fmt.Errorf("wrapped: %w", err), &apiErr) {
						t.Fatalf("missing ApiError: %T", err)
					}
					if apiErr.HTTPStatus != status || apiErr.Code != status || apiErr.Status != status || apiErr.BusinessCode != body.code {
						t.Fatalf("diagnostics/mapping = %+v", apiErr)
					}
				})
			}
		}
	}
}

func TestSSEErrorDiagnostics(t *testing.T) {
	cases := []struct {
		name, event, body, code, msg string
		status                       int
	}{
		{"flat queue", "", `{"error_code":"InferHub.ModelArts.81111.429","error_msg":"TPM limit reached"}`, "InferHub.ModelArts.81111.429", "TPM limit reached", 429},
		{"nested queue", "", `{"error":{"code":429,"message":"quota exceeded"}}`, "429", "quota exceeded", 429},
		{"flat model", "done", `{"error_code":"InferHub.002002009","error_msg":"not registered"}`, "InferHub.002002009", "not registered", 400},
		{"nested benefit", "error", `{"error":{"code":"InferHub.4004.200","message":"benefit not found"}}`, "InferHub.4004.200", "benefit not found", 400},
		{"nested parameter", "message", `{"error":{"code":"InferHub.001001005","message":"request param is invalid"}}`, "InferHub.001001005", "request param is invalid", 400},
		{"unknown flat", "", `{"error_code":"new.business.code","error_msg":"unknown failure"}`, "new.business.code", "unknown failure", 502},
		{"unknown numeric parameter", "", `{"error_code":400,"error_msg":"unknown failure"}`, "400", "unknown failure", 502},
		{"unknown numeric auth", "", `{"error":{"code":401,"message":"unknown failure"}}`, "401", "unknown failure", 502},
		{"unknown numeric permission", "", `{"error":{"code":403,"message":"unknown failure"}}`, "403", "unknown failure", 502},
		{"unknown numeric", "onAnswer", `{"error_code":9007199254740993,"error_msg":"unknown failure"}`, "9007199254740993", "unknown failure", 502},
		{"unknown nested", "reasoning", `{"error":{"code":"new.business.code","message":"unknown failure"}}`, "new.business.code", "unknown failure", 502},
		{"nested without code", "", `{"error":{"message":"unknown failure"}}`, "", "unknown failure", 502},
		{"nested object code", "error", `{"error":{"code":{"secret":"value"},"message":"unknown failure"}}`, "", "unknown failure", 502},
		{"explicit unknown", "error", `unexpected format`, "", "request failed", 502},
		{"legacy code", "done", `{"code":"legacy.failure","message":"legacy message"}`, "legacy.failure", "legacy message", 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := "data: " + tc.body + "\n\n"
			if tc.event != "" {
				frame = "event: " + tc.event + "\n" + frame
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if r.URL.Query().Get("middle") == "1" {
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
				}
				_, _ = io.WriteString(w, frame)
			}))
			defer srv.Close()
			c := NewWithChatEndpoint(time.Second, srv.URL)
			_, err := c.SendChatV2(context.Background(), map[string]any{}, "trace", SignCredential{}, "token", false)
			if err == nil {
				t.Fatal("first-frame error accepted")
			}
			var apiErr *ApiError
			if !errors.As(fmt.Errorf("wrapped: %w", err), &apiErr) || apiErr.HTTPStatus != 200 || apiErr.BusinessCode != tc.code || apiErr.Status != tc.status || apiErr.Code != tc.status {
				t.Fatalf("first-frame diagnostics = %+v (%v)", apiErr, err)
			}

			c.chatURL = srv.URL + "?middle=1"
			for _, mode := range []string{"aggregate", "stream"} {
				t.Run(mode, func(t *testing.T) {
					stream, err := c.SendChatV2(context.Background(), map[string]any{}, "trace", SignCredential{}, "token", false)
					if err != nil {
						t.Fatalf("preflight rejected successful first frame: %v", err)
					}
					defer stream.Close()
					var streamErr error
					if mode == "aggregate" {
						_, streamErr = AggregateRaw(stream)
					} else {
						rec := httptest.NewRecorder()
						called := false
						streamErr = StreamCapture(rec, stream, "GLM-5.3", func(*RawCompletion) { called = true })
						if called || strings.Count(rec.Body.String(), "event: error\n") != 1 || strings.Count(rec.Body.String(), "data: [DONE]") != 1 || !strings.Contains(rec.Body.String(), `"content":"ok"`) {
							t.Fatalf("terminal error stream = %s; success callback = %v", rec.Body.String(), called)
						}
					}
					if streamErr == nil {
						t.Fatal("midstream error accepted")
					}
					var upErr *UpstreamError
					if !errors.As(fmt.Errorf("wrapped: %w", streamErr), &upErr) || upErr.HTTPStatus != 200 || upErr.Code != tc.code || upErr.Msg != tc.msg {
						t.Fatalf("midstream diagnostics = %+v (%v)", upErr, streamErr)
					}
				})
			}
		})
	}
}

func TestPreflightSSEReplaysSuccessfulStream(t *testing.T) {
	for _, terminal := range []string{`{"error_code":"0"}`, `{"error_code":0}`, `{"error":null}`, `{"error":{"code":0}}`} {
		t.Run(terminal, func(t *testing.T) {
			input := ": heartbeat\r\n\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"hello\",\"reasoning_content\":\"think\"}}]}\r\n\r\nevent: done\r\ndata: " + terminal + "\r\n\r\n"
			r, err := preflightSSE(io.NopCloser(strings.NewReader(input)))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			raw, err := io.ReadAll(r)
			if err != nil || string(raw) != input {
				t.Fatalf("preflight changed successful bytes: %q, %v", raw, err)
			}
			rc, err := AggregateRaw(strings.NewReader(string(raw)))
			if err != nil || rc.Content != "hello" || rc.Reasoning != "think" || rc.Finish != "stop" {
				t.Fatalf("aggregate = %+v, %v", rc, err)
			}
			rec := httptest.NewRecorder()
			var captured *RawCompletion
			err = StreamCapture(rec, strings.NewReader(string(raw)), "GLM-5.2", func(rc *RawCompletion) { captured = rc })
			if err != nil || captured == nil || captured.Content != "hello" || captured.Reasoning != "think" || strings.Contains(rec.Body.String(), "event: error") || strings.Count(rec.Body.String(), "data: [DONE]") != 1 {
				t.Fatalf("successful stream regressed: captured=%+v, err=%v, body=%s", captured, err, rec.Body.String())
			}
		})
	}
}

func TestStreamCaptureErrorFrameContainsRawErrorMessage(t *testing.T) {
	// Document the existing public wire behavior. The executor's frameWriter must
	// suppress this frame before emitting its own redacted diagnostic.
	stream := "data: {\"error_code\":\"secret-code\",\"error_msg\":\"secret-token\"}\n\n"
	rec := httptest.NewRecorder()
	err := StreamCapture(rec, strings.NewReader(stream), "GLM-5.3", nil)
	want := "event: error\ndata: {\"error\":{\"code\":\"CODEARTS_STREAM_ERROR\",\"message\":\"codearts error code=secret-code msg=secret-token\",\"type\":\"upstream_error\"}}\n\ndata: [DONE]\n\n"
	if err == nil || rec.Body.String() != want {
		t.Fatalf("existing error frame changed: %q, %v", rec.Body.String(), err)
	}
}
