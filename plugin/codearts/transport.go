package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Host callbacks use the same wire contract for both short HTTP calls and SSE.
// The injected boundary keeps upstream.Client's signing and preflight intact.
var executorHostCall = hostCall

type bridgeHTTPResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	Body       []byte              `json:"body"`
	StreamID   string              `json:"stream_id"`
	Chunks     []struct {
		Payload []byte `json:"payload"`
	} `json:"chunks"`
}

func (r *bridgeHTTPResponse) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, pair := range []struct {
		names []string
		dest  any
	}{
		{[]string{"status_code", "StatusCode", "statusCode"}, &r.StatusCode},
		{[]string{"headers", "Headers"}, &r.Headers},
		{[]string{"body", "Body"}, &r.Body},
		{[]string{"stream_id", "StreamID", "streamId"}, &r.StreamID},
		{[]string{"chunks", "Chunks"}, &r.Chunks},
	} {
		for _, name := range pair.names {
			if v, ok := fields[name]; ok {
				if err := json.Unmarshal(v, pair.dest); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

type hostRoundTripper struct{ stream bool }

func (t hostRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	wire, err := json.Marshal(map[string]any{"request": map[string]any{
		"method": req.Method, "url": req.URL.String(), "headers": map[string][]string(req.Header), "body": body,
	}})
	if err != nil {
		return nil, err
	}
	method := "host.http.do"
	if t.stream {
		method = "host.http.do_stream"
	}
	raw, err := executorHostCall(method, wire)
	if err != nil {
		return nil, err
	}
	result, err := hostUnwrap(raw)
	if err != nil {
		return nil, err
	}
	var reply bridgeHTTPResponse
	if err := json.Unmarshal(result, &reply); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if reply.StatusCode <= 0 {
		return nil, fmt.Errorf("%s: missing status_code", method)
	}
	var reader io.ReadCloser = io.NopCloser(bytes.NewReader(reply.Body))
	if reply.StreamID != "" {
		reader = newBridgeStream(req.Context(), reply.StreamID, reply.Chunks)
	} else if len(reply.Chunks) > 0 {
		var initial bytes.Buffer
		for _, chunk := range reply.Chunks {
			initial.Write(chunk.Payload)
		}
		reader = io.NopCloser(bytes.NewReader(initial.Bytes()))
	} else if t.stream && reply.StatusCode < 400 && reply.Body == nil {
		return nil, fmt.Errorf("host.http.do_stream: missing stream_id")
	}
	return &http.Response{StatusCode: reply.StatusCode, Header: http.Header(reply.Headers), Body: reader, Request: req}, nil
}

type bridgeStream struct {
	ctx     context.Context
	id      string
	mu      sync.Mutex
	initial [][]byte
	pending []byte
	closed  bool
	done    bool
	stop    chan struct{}
	once    sync.Once
}

func newBridgeStream(ctx context.Context, id string, chunks []struct {
	Payload []byte `json:"payload"`
}) *bridgeStream {
	s := &bridgeStream{ctx: ctx, id: id, stop: make(chan struct{})}
	for _, chunk := range chunks {
		s.initial = append(s.initial, chunk.Payload)
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.stop:
		}
	}()
	return s
}

func (s *bridgeStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := s.ctx.Err(); err != nil {
			return 0, err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return 0, io.EOF
		}
		if len(s.pending) == 0 && len(s.initial) > 0 {
			s.pending, s.initial = s.initial[0], s.initial[1:]
		}
		if len(s.pending) > 0 {
			n := copy(p, s.pending)
			s.pending = s.pending[n:]
			s.mu.Unlock()
			return n, nil
		}
		if s.done {
			s.mu.Unlock()
			return 0, io.EOF
		}
		s.mu.Unlock()
		raw, err := executorHostCall("host.http.stream_read", []byte(`{"stream_id":`+jsonString(s.id)+`}`))
		if err != nil {
			return 0, err
		}
		result, err := hostUnwrap(raw)
		if err != nil {
			return 0, err
		}
		var next struct {
			Payload []byte `json:"payload"`
			Error   string `json:"error"`
			Done    bool   `json:"done"`
		}
		if err := json.Unmarshal(result, &next); err != nil {
			return 0, err
		}
		if next.Error != "" {
			return 0, fmt.Errorf("host stream read: %s", next.Error)
		}
		if len(next.Payload) > 0 {
			s.mu.Lock()
			s.pending = next.Payload
			s.done = next.Done
			s.mu.Unlock()
			continue
		}
		if next.Done {
			return 0, io.EOF
		}
	}
}

func (s *bridgeStream) Close() error {
	s.once.Do(func() {
		close(s.stop)
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		_, _ = executorHostCall("host.http.stream_close", []byte(`{"stream_id":`+jsonString(s.id)+`}`))
	})
	return nil
}

func jsonString(v string) string { b, _ := json.Marshal(v); return string(b) }

func hostStreamCall(method, id string, payload []byte) error {
	request := map[string]any{"stream_id": id}
	if payload != nil {
		request["payload"] = payload
	}
	return hostStreamRequest(method, request)
}

// hostStreamClose ends an async stream. A non-empty message becomes the host's
// terminal stream error, so passing it lets the host report the failure instead
// of treating a truncated stream as a successful one.
func hostStreamClose(id, message string) error {
	request := map[string]any{"stream_id": id}
	if message != "" {
		request["error"] = message
	}
	return hostStreamRequest("host.stream.close", request)
}

func hostStreamRequest(method string, request map[string]any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	raw, err := executorHostCall(method, body)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		_, err = hostUnwrap(raw)
	}
	return err
}
