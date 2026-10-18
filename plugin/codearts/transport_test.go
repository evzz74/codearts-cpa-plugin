package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestHostTransportShortAndStream(t *testing.T) {
	old := executorHostCall
	defer func() { executorHostCall = old }()
	var mu sync.Mutex
	var calls []string
	executorHostCall = func(method string, raw []byte) ([]byte, error) {
		mu.Lock()
		calls = append(calls, method)
		mu.Unlock()
		switch method {
		case "host.http.do", "host.http.do_stream":
			var wire struct {
				Request struct {
					Method  string              `json:"method"`
					URL     string              `json:"url"`
					Body    []byte              `json:"body"`
					Headers map[string][]string `json:"headers"`
				} `json:"request"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Error(err)
			}
			if wire.Request.Method != "POST" || string(wire.Request.Body) != "hello" || wire.Request.Headers["Authorization"][0] != "secret" {
				t.Errorf("incorrect wire: %+v", wire)
			}
			if method == "host.http.do" {
				return okEnvelope(map[string]any{"status_code": 201, "body": []byte("short")}), nil
			}
			return okEnvelope(map[string]any{"status_code": 200, "stream_id": "upstream", "chunks": []map[string]any{{"payload": []byte("one")}}}), nil
		case "host.http.stream_read":
			return okEnvelope(map[string]any{"payload": []byte("two"), "done": true}), nil
		case "host.http.stream_close":
			return okEnvelope(map[string]any{}), nil
		default:
			t.Errorf("unexpected method %s", method)
			return nil, nil
		}
	}
	for _, stream := range []bool{false, true} {
		req, _ := http.NewRequestWithContext(context.Background(), "POST", "https://example.test/chat", strings.NewReader("hello"))
		req.Header.Set("Authorization", "secret")
		resp, err := (hostRoundTripper{stream: stream}).RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if !stream && (resp.StatusCode != 201 || string(body) != "short") {
			t.Fatalf("short response: %d %s", resp.StatusCode, body)
		}
		if stream && (resp.StatusCode != 200 || string(body) != "onetwo") {
			t.Fatalf("stream response: %d %s", resp.StatusCode, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 || calls[3] != "host.http.stream_close" {
		t.Fatalf("host calls: %v", calls)
	}
}
