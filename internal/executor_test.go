package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"
)

func testExecutorRequest(t *testing.T, body string) []byte {
	t.Helper()
	raw, err := json.Marshal(executorRequest{Model: "GLM-5.2", StorageJSON: []byte(`{"user_id":"selected","cloud_dragon_token":"selected-token","access_key_id":"ak","secret_access_key":"sk"}`), Payload: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestExecutorNativeToolsAndHistory(t *testing.T) {
	raw := testExecutorRequest(t, `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"x\":1}"}}]},{"role":"tool","tool_call_id":"call_1","content":"found"}],"tools":[{"type":"function","function":{"name":"lookup"}},{"type":"function","function":{"name":"other"}}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`)
	_, messages, opts, id, cred, err := parseExecutor(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[1].ToolCalls[0].ID != "call_1" || messages[2].ToolCallID != "call_1" || opts.ToolChoice != "required" || len(opts.Tools) != 1 || id == "" || cred.SecurityToken != "selected-token" {
		t.Fatalf("messages=%+v opts=%+v id=%q cred=%+v", messages, opts, id, cred)
	}
	if _, _, _, id2, _, err := parseExecutor(raw); err != nil || id != id2 {
		t.Fatalf("unstable chat ID: %q, %q: %v", id, id2, err)
	}
}

func TestExecutorCompletionAndStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var chat map[string]any
		_ = json.Unmarshal(body, &chat)
		if r.Header.Get("X-Auth-Token") != "selected-token" || chat["tool_choice"] != "required" || len(chat["messages"].([]any)) != 3 || len(chat["tools"].([]any)) != 1 {
			t.Errorf("incorrect upstream request headers=%v body=%s", r.Header, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\",\"tool_calls\":[{\"index\":0,\"id\":\"call_2\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"2}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	old := executorClient
	executorClient = func() *upstream.Client { return upstream.NewWithChatEndpoint(time.Second*3, server.URL) }
	defer func() { executorClient = old }()
	raw := testExecutorRequest(t, `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}],"tools":[{"type":"function","function":{"name":"lookup"}},{"type":"function","function":{"name":"other"}}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`)
	result, err := handleExecute(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result struct {
			Payload []byte `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &env); err != nil {
		t.Fatal(err)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				ReasoningContent string                  `json:"reasoning_content"`
				ToolCalls        []upstream.ChatToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(env.Result.Payload, &completion); err != nil {
		t.Fatal(err)
	}
	if len(completion.Choices) == 0 || completion.Choices[0].Message.ReasoningContent != "think" || len(completion.Choices[0].Message.ToolCalls) != 1 || completion.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"x":2}` {
		t.Fatalf("completion: %s", env.Result.Payload)
	}
	stream, err := handleExecuteStream(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stream, []byte(`"chunks"`)) || !bytes.Contains(stream, []byte(`"payload"`)) {
		t.Fatalf("stream: %s", stream)
	}
}

func TestExecutorAsyncEmitAndClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	oldClient, oldCall := executorClient, executorHostCall
	executorClient = func() *upstream.Client { return upstream.NewWithChatEndpoint(3*time.Second, server.URL) }
	defer func() { executorClient, executorHostCall = oldClient, oldCall }()
	calls := make(chan string, 8)
	executorHostCall = func(method string, raw []byte) ([]byte, error) {
		if method == "host.stream.emit" || method == "host.stream.close" {
			var v struct {
				StreamID string `json:"stream_id"`
				Payload  []byte `json:"payload"`
			}
			if err := json.Unmarshal(raw, &v); err != nil || v.StreamID != "response-stream" {
				t.Errorf("callback: %s %s %v", method, raw, err)
			}
			calls <- method + ":" + string(v.Payload)
		}
		return okEnvelope(map[string]any{}), nil
	}
	var req executorRequest
	if err := json.Unmarshal(testExecutorRequest(t, `{"messages":[{"role":"user","content":"hi"}]}`), &req); err != nil {
		t.Fatal(err)
	}
	req.StreamID = "response-stream"
	req.Metadata = map[string]any{"request_path": "/v1/chat/completions"}
	raw, _ := json.Marshal(req)
	response, err := handleExecuteStream(raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(response, []byte(`"payload"`)) {
		t.Fatalf("async returned chunks: %s", response)
	}
	select {
	case event := <-calls:
		if !strings.HasPrefix(event, "host.stream.emit:") || !strings.Contains(event, "hello") {
			t.Fatalf("event: %s", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no async emit")
	}
	for {
		select {
		case event := <-calls:
			if event == "host.stream.close:" {
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no async close")
		}
	}
}

func TestExecutorFraming(t *testing.T) {
	for _, tc := range []struct {
		path   string
		framed bool
	}{{"/v1/chat/completions", false}, {"/v1/messages", true}, {"", true}} {
		if got := executorNeedsFrame(map[string]any{"request_path": tc.path}); got != tc.framed {
			t.Fatalf("%s framing=%v", tc.path, got)
		}
		var chunks [][]byte
		err := streamFrames(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), "GLM-5.2", tc.framed, func(b []byte) error { chunks = append(chunks, b); return nil })
		if err != nil || len(chunks) == 0 {
			t.Fatalf("chunks=%q err=%v", chunks, err)
		}
		if strings.HasPrefix(string(chunks[0]), "data: ") != tc.framed {
			t.Fatalf("framing mismatch: %q", chunks[0])
		}
	}
}
