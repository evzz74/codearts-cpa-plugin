package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"
)

type executorRequest struct {
	AuthID          string
	Model           string
	StorageJSON     []byte
	Payload         []byte
	OriginalRequest []byte
	Metadata        map[string]any
	StreamID        string `json:"stream_id"`
	HostCallbackID  string `json:"host_callback_id"`
}

type executorChunk struct {
	Payload []byte `json:"payload"`
}
type executorStreamResponse struct {
	Headers http.Header     `json:"headers"`
	Chunks  []executorChunk `json:"chunks"`
}

// The CPA supplies the selected account's storage, never a global account pool.
var executorClient = func() *upstream.Client {
	return newUpstreamClient(120 * time.Second)
}

func newUpstreamClient(timeout time.Duration) *upstream.Client {
	client := upstream.New(timeout)
	if hostAPI != nil && runtime.GOOS != "windows" {
		client.SetTransport(hostRoundTripper{stream: false})
	}
	return client
}

func parseExecutor(raw []byte) (executorRequest, []upstream.ChatMessage, upstream.ChatOptions, string, upstream.SignCredential, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, err
	}
	if req.AuthID != "" {
		file, err := findAccountByID(req.AuthID)
		if err != nil {
			return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, newPluginError("auth_store_unavailable", http.StatusBadGateway, "could not resolve selected account")
		}
		_, storage, err := accountStorage(file)
		if err != nil {
			return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, newPluginError("auth_store_unavailable", http.StatusBadGateway, "could not read selected account")
		}
		req.StorageJSON = storage
	}
	acct, err := authFromStorage(req.StorageJSON)
	if err != nil {
		return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, authStorageError(err)
	}
	if _, disabled := authMetadata(req.StorageJSON); disabled {
		return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, newPluginError("auth_disabled", http.StatusForbidden, "account is disabled")
	}
	if acct.AccessKeyID == "" || acct.SecretAccessKey == "" {
		return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, newPluginError("auth_unusable", http.StatusForbidden, "missing account signing credentials")
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	var chat struct {
		Model               string            `json:"model"`
		Messages            []json.RawMessage `json:"messages"`
		ConversationID      string            `json:"conversation_id"`
		ReasoningEffort     string            `json:"reasoning_effort"`
		MaxTokens           *int              `json:"max_tokens"`
		MaxCompletionTokens *int              `json:"max_completion_tokens"`
		Temperature         *float64          `json:"temperature"`
		TopP                *float64          `json:"top_p"`
		Tools               []map[string]any  `json:"tools"`
		ToolChoice          json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, fmt.Errorf("decode chat payload: %w", err)
	}
	if len(chat.Messages) == 0 {
		return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, fmt.Errorf("messages is empty")
	}
	messages := make([]upstream.ChatMessage, 0, len(chat.Messages))
	var anchor string
	for _, rawMessage := range chat.Messages {
		var msg struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			ReasoningContent *string         `json:"reasoning_content"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
			ToolCallID       string          `json:"tool_call_id"`
			Name             string          `json:"name"`
		}
		if err := json.Unmarshal(rawMessage, &msg); err != nil {
			return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, err
		}
		switch msg.Role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, fmt.Errorf("invalid message role %q", msg.Role)
		}
		if anchor == "" && msg.Role == "user" {
			anchor = string(rawMessage)
		}
		var content any
		if len(msg.Content) > 0 {
			if err := json.Unmarshal(msg.Content, &content); err != nil {
				return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, err
			}
		}
		if parts, ok := content.([]any); ok {
			var text strings.Builder
			for _, part := range parts {
				if item, ok := part.(map[string]any); ok {
					if value, ok := item["text"].(string); ok {
						text.WriteString(value)
					}
				}
			}
			content = text.String()
		}
		var calls []upstream.ChatToolCall
		if len(msg.ToolCalls) > 0 && string(msg.ToolCalls) != "null" {
			var rawCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			}
			if err := json.Unmarshal(msg.ToolCalls, &rawCalls); err != nil {
				return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, err
			}
			for _, call := range rawCalls {
				args := "{}"
				if len(call.Function.Arguments) > 0 && string(call.Function.Arguments) != "null" {
					if json.Unmarshal(call.Function.Arguments, &args) != nil {
						args = string(call.Function.Arguments)
					}
				}
				calls = append(calls, upstream.ChatToolCall{ID: call.ID, Type: "function", Function: upstream.ChatToolFunction{Name: call.Function.Name, Arguments: args}})
			}
		}
		role := msg.Role
		if role == "developer" {
			role = "system"
		}
		if role == "tool" && content == "" {
			content = "(no output)"
		}
		messages = append(messages, upstream.ChatMessage{Role: role, Content: content, ReasoningContent: msg.ReasoningContent, ToolCalls: calls, ToolCallID: msg.ToolCallID, Name: msg.Name})
	}
	messages = completeToolHistory(messages)
	model := req.Model
	if model == "" {
		model = chat.Model
	}
	if model == "" {
		return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, fmt.Errorf("model is empty")
	}
	if modelDisabled(req.StorageJSON, model) {
		return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, newPluginError("model_disabled", http.StatusForbidden, "model is disabled for this account")
	}
	opts := upstream.ChatOptions{ReasoningEffort: chat.ReasoningEffort, MaxTokens: chat.MaxTokens, Temperature: chat.Temperature, TopP: chat.TopP, Tools: chat.Tools}
	if chat.MaxCompletionTokens != nil {
		opts.MaxTokens = chat.MaxCompletionTokens
	}
	if len(chat.ToolChoice) > 0 && string(chat.ToolChoice) != "null" {
		var mode string
		if json.Unmarshal(chat.ToolChoice, &mode) == nil {
			switch mode {
			case "none", "auto", "required":
				opts.ToolChoice = mode
			default:
				return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, fmt.Errorf("invalid tool_choice %q", mode)
			}
		} else {
			var named struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(chat.ToolChoice, &named) != nil || named.Type != "function" || named.Function.Name == "" {
				return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, fmt.Errorf("invalid tool_choice")
			}
			var selected map[string]any
			for _, tool := range chat.Tools {
				fn, _ := tool["function"].(map[string]any)
				if fn["name"] == named.Function.Name {
					if selected != nil {
						return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, fmt.Errorf("duplicate named tool %q", named.Function.Name)
					}
					selected = tool
				}
			}
			if selected == nil {
				return req, nil, upstream.ChatOptions{}, "", upstream.SignCredential{}, fmt.Errorf("named tool %q not found", named.Function.Name)
			}
			opts.Tools = []map[string]any{selected}
			opts.ToolChoice = "required" // InferHub rejects object-form tool_choice.
		}
	}
	chatID := strings.TrimSpace(chat.ConversationID)
	if chatID == "" {
		if anchor == "" {
			anchor = string(chat.Messages[0])
		}
		if len(chat.Tools) > 0 {
			anchor += "\x00tools"
		}
		sum := sha256.Sum256([]byte(anchor))
		chatID = hex.EncodeToString(sum[:16])
	}
	cred := upstream.SignCredential{SecurityToken: acct.CloudDragonTok, AccessKeyID: acct.AccessKeyID, SecretAccessKey: acct.SecretAccessKey}
	return req, messages, opts, chatID, cred, nil
}

func completeToolHistory(messages []upstream.ChatMessage) []upstream.ChatMessage {
	results := make(map[string]bool)
	for _, message := range messages {
		if message.Role == "tool" && message.ToolCallID != "" {
			results[message.ToolCallID] = true
		}
	}
	paired := make(map[string]bool)
	for i := range messages {
		if messages[i].Role != "assistant" || len(messages[i].ToolCalls) == 0 {
			continue
		}
		complete := true
		for _, call := range messages[i].ToolCalls {
			if call.ID == "" || !results[call.ID] {
				complete = false
				break
			}
		}
		if complete {
			for _, call := range messages[i].ToolCalls {
				paired[call.ID] = true
			}
		} else {
			messages[i].ToolCalls = nil
		}
	}
	filtered := make([]upstream.ChatMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role == "tool" && !paired[message.ToolCallID] {
			continue
		}
		if message.Role == "assistant" && len(message.ToolCalls) == 0 && message.Content == nil {
			continue
		}
		filtered = append(filtered, message)
	}
	return filtered
}

func openExecutorStream(ctx context.Context, raw []byte) (executorRequest, io.ReadCloser, string, error) {
	req, messages, opts, chatID, cred, err := parseExecutor(raw)
	if err != nil {
		return req, nil, "", err
	}
	model := req.Model
	if model == "" {
		var chat struct {
			Model string `json:"model"`
		}
		body := req.Payload
		if len(body) == 0 {
			body = req.OriginalRequest
		}
		_ = json.Unmarshal(body, &chat)
		model = chat.Model
	}
	model = upstreamModelName(model)
	client := executorClient()
	// The short client handles other CodeArts calls; SSE uses an unbounded host stream.
	if hostAPI != nil && runtime.GOOS != "windows" {
		client.SetTransport(hostRoundTripper{stream: true})
	}
	acct, _ := authFromStorage(req.StorageJSON)
	stream, err := client.ChatStreamWithOptions(ctx, chatID, messages, "", cred, acct.UserName, model, opts, upstream.IsBenefitModel(acct.UserID, model))
	return req, stream, model, err
}

// AuthID and AuthIndex are distinct host identifiers; device_id is not an account key.
func usageFromRequest(req executorRequest) (device, account, alias string) {
	if req.AuthID != "" {
		if file, err := findAccountByID(req.AuthID); err == nil {
			device = file.AuthIndex
		} else {
			device = "unresolved-" + req.AuthID
		}
	}
	acct, err := authFromStorage(req.StorageJSON)
	if err != nil {
		return device, "", req.Model
	}
	account = acct.UserName
	if account == "" {
		account = acct.UserID
	}
	alias = req.Model
	if value, ok := req.Metadata["model"].(string); ok && value != "" {
		alias = value
	}
	if device == "" {
		device = acct.UserID
	}
	return device, account, alias
}

// buildMessagesText 把请求消息拍平成一段文本，仅用于无 usage 时的估算。
func buildMessagesText(messages []upstream.ChatMessage) string {
	var sb strings.Builder
	for _, message := range messages {
		switch content := message.Content.(type) {
		case string:
			sb.WriteString(content)
		case nil:
		default:
			raw, _ := json.Marshal(content)
			sb.Write(raw)
		}
		if message.ReasoningContent != nil {
			sb.WriteString(*message.ReasoningContent)
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		for _, call := range message.ToolCalls {
			sb.WriteString(call.Function.Name)
			sb.WriteString(call.Function.Arguments)
		}
	}
	return sb.String()
}

func handleExecute(raw []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return executeWithContext(ctx, raw)
}

func executeWithContext(ctx context.Context, raw []byte) ([]byte, error) {
	started := time.Now()
	var req executorRequest
	_ = json.Unmarshal(raw, &req)
	_, stream, model, err := openExecutorStream(ctx, raw)
	if err != nil {
		recordUsageExecution(req, model, started, nil, false, err)
		return nil, safeExecutionError(err)
	}
	defer stream.Close()
	completion, err := upstream.Aggregate(stream, model)
	if err != nil {
		recordUsageExecution(req, model, started, nil, false, err)
		return nil, safeExecutionError(err)
	}
	usage, _ := completion["usage"].(map[string]any)
	var captured upstream.RawCompletion
	if choices, ok := completion["choices"].([]any); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		captured.Content, _ = message["content"].(string)
		captured.Reasoning, _ = message["reasoning_content"].(string)
		captured.ToolCalls, _ = message["tool_calls"].([]upstream.ChatToolCall)
	}
	recordUsageExecution(req, model, started, usage, false, nil, &captured)
	payload, err := json.Marshal(completion)
	if err != nil {
		return nil, err
	}
	return okEnvelope(map[string]any{"payload": payload}), nil
}

// recordUsageExecution 把一次请求计入账本。任何异常都被吞掉：统计不能影响请求。
func recordUsageExecution(req executorRequest, model string, started time.Time, usage map[string]any, streamed bool, callErr error, completion ...*upstream.RawCompletion) {
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = recovered
		}
	}()
	device, account, alias := usageFromRequest(req)
	if model == "" {
		model = upstreamModelName(alias)
	}
	entry := usageEntry{
		At:        started,
		Device:    device,
		Account:   account,
		Model:     model,
		Stream:    streamed,
		LatencyMS: time.Since(started).Milliseconds(),
	}
	if callErr != nil {
		entry.Failed = true
		entry.Diagnostic = failureDetails(callErr)
		if typed, ok := safeExecutionError(callErr).(*pluginError); ok {
			entry.Status = typed.status
		}
		recordUsage(entry)
		return
	}
	if input, output, reasoning, total, ok := usageCountsFromRaw(usage); ok {
		entry.Input, entry.Output, entry.Reasoning, entry.Total = input, output, reasoning, total
		recordUsage(entry)
		return
	}
	// 上游没给 usage：按请求与响应正文估算，并在明细里标 estimated。
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	var chat struct {
		Messages []struct {
			Content          any             `json:"content"`
			ReasoningContent *string         `json:"reasoning_content"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
		Tools json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(body, &chat) == nil {
		var text strings.Builder
		for _, message := range chat.Messages {
			text.WriteString(buildMessagesText([]upstream.ChatMessage{{Content: message.Content, ReasoningContent: message.ReasoningContent}}))
			if len(message.ToolCalls) > 0 && string(message.ToolCalls) != "null" {
				text.Write(message.ToolCalls)
			}
		}
		if len(chat.Tools) > 0 && string(chat.Tools) != "null" {
			text.Write(chat.Tools)
		}
		entry.Input = estimateFromText(text.String())
	}
	if len(completion) > 0 && completion[0] != nil {
		entry.Reasoning = estimateFromText(completion[0].Reasoning)
		entry.Output = estimateFromText(buildMessagesText([]upstream.ChatMessage{{Content: completion[0].Content, ReasoningContent: &completion[0].Reasoning, ToolCalls: completion[0].ToolCalls}}))
	}
	entry.Estimated = true
	entry.Total = entry.Input + entry.Output
	recordUsage(entry)
}

func streamFrames(stream io.Reader, model string, framed bool, emit func([]byte) error) error {
	return streamFramesCapture(stream, model, framed, emit, nil)
}

// streamFramesCapture 与 streamFrames 相同，但把聚合结果（含 usage 帧）
// 交回给调用方，供用量账本与估算使用。
func streamFramesCapture(stream io.Reader, model string, framed bool, emit func([]byte) error, done func(*upstream.RawCompletion)) error {
	w := &frameWriter{framed: framed, emit: emit}
	return upstream.StreamCapture(w, stream, model, done)
}

// upstream.Stream writes SSE records. CPA's native OpenAI path frames raw JSON
// itself, while cross-format translators require a complete data: record.
type frameWriter struct {
	pending []byte
	framed  bool
	emit    func([]byte) error
	header  http.Header
}

func (w *frameWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *frameWriter) WriteHeader(int) {}

func (w *frameWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	for {
		end := bytes.Index(w.pending, []byte("\n\n"))
		if end < 0 {
			break
		}
		record := string(w.pending[:end])
		w.pending = w.pending[end+2:]
		if strings.Contains(record, "data: [DONE]") {
			continue
		} // host terminates streams
		if strings.HasPrefix(record, "data: ") {
			payload := []byte(strings.TrimPrefix(record, "data: "))
			if w.framed {
				payload = []byte("data: " + string(payload))
			}
			if err := w.emit(payload); err != nil {
				return 0, err
			}
		}
		// StreamCapture returns the typed failure; only the caller emits its safe summary.
	}
	return len(p), nil
}

func executorNeedsFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

func handleExecuteStream(raw []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Validate synchronously before returning an async stream to the host.
	started := time.Now()
	if _, _, _, _, _, err := parseExecutor(raw); err != nil {
		if req.AuthID != "" || len(req.StorageJSON) > 0 {
			recordUsageExecution(req, "", started, nil, true, err)
		}
		return nil, safeExecutionError(err)
	}
	response := executorStreamResponse{Headers: http.Header{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}}, Chunks: []executorChunk{}}
	framed := executorNeedsFrame(req.Metadata)
	if req.StreamID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		started := time.Now()
		_, stream, model, err := openExecutorStream(ctx, raw)
		if err != nil {
			recordUsageExecution(req, model, started, nil, true, err)
			return nil, safeExecutionError(err)
		}
		defer stream.Close()
		var captured *upstream.RawCompletion
		err = streamFramesCapture(stream, model, framed, func(payload []byte) error {
			response.Chunks = append(response.Chunks, executorChunk{Payload: payload})
			return nil
		}, func(done *upstream.RawCompletion) { captured = done })
		if err != nil {
			recordUsageExecution(req, model, started, nil, true, err)
			return nil, safeExecutionError(err)
		}
		var usage map[string]any
		if captured != nil {
			usage = captured.Usage
		}
		recordUsageExecution(req, model, started, usage, true, nil, captured)
		return okEnvelope(response), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		started := time.Now()
		var failure string
		var captured *upstream.RawCompletion
		_, stream, model, err := openExecutorStream(ctx, raw)
		if err == nil {
			err = streamFramesCapture(stream, model, framed, func(payload []byte) error { return hostStreamCall("host.stream.emit", req.StreamID, payload) }, func(done *upstream.RawCompletion) { captured = done })
			_ = stream.Close()
		}
		if err != nil && ctx.Err() == nil {
			diagnostic := failureDetails(err)
			safe := safeExecutionError(err).(*pluginError)
			failure = "codearts stream failed: " + diagnostic.summary()
			message, _ := json.Marshal(map[string]any{"error": map[string]any{"message": failure, "type": "upstream_error", "status": safe.status, "code": safe.code, "diagnostic": diagnostic}})
			if framed {
				message = []byte("event: error\ndata: " + string(message) + "\n\n")
			}
			_ = hostStreamCall("host.stream.emit", req.StreamID, message)
		}
		// 客户端中断也算一次失败请求：账本要能解释「为什么没消耗」。
		recordErr := err
		if recordErr == nil && failure != "" {
			recordErr = fmt.Errorf("%s", failure)
		}
		var usage map[string]any
		if captured != nil {
			usage = captured.Usage
		}
		recordUsageExecution(req, model, started, usage, true, recordErr, captured)
		_ = hostStreamClose(req.StreamID, failure)
	}()
	return okEnvelope(response), nil
}
