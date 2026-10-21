package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

func disabledModels(storage []byte) []string {
	var policy struct {
		Disabled []string `json:"disabled_models"`
	}
	_ = json.Unmarshal(storage, &policy)
	return policy.Disabled
}

func modelDisabled(storage []byte, model string) bool {
	for _, disabled := range disabledModels(storage) {
		if strings.EqualFold(upstreamModelName(disabled), upstreamModelName(model)) {
			return true
		}
	}
	return false
}

type modelTestResult struct {
	OK         bool               `json:"ok"`
	CheckedAt  time.Time          `json:"checked_at"`
	LatencyMS  int64              `json:"latency_ms"`
	Message    string             `json:"message"`
	Diagnostic *failureDiagnostic `json:"diagnostic,omitempty"`
}

type modelTestState struct {
	running bool
	last    time.Time
	results map[string]modelTestResult
}

var modelTests = struct {
	sync.Mutex
	accounts map[string]*modelTestState
}{accounts: map[string]*modelTestState{}}

func managementModelAccount(index string) (accountFile, *Auth, []byte, int) {
	if index == "" {
		return accountFile{}, nil, nil, http.StatusBadRequest
	}
	files, err := selectedAccounts(index)
	if err != nil {
		return accountFile{}, nil, nil, http.StatusBadGateway
	}
	if len(files) != 1 {
		return accountFile{}, nil, nil, http.StatusNotFound
	}
	acct, storage, err := accountStorage(files[0])
	if err != nil {
		return accountFile{}, nil, nil, http.StatusBadGateway
	}
	return files[0], acct, storage, http.StatusOK
}

func managementModels(index string) []byte {
	_, acct, storage, status := managementModelAccount(index)
	if status != http.StatusOK {
		return managementJSON(status, map[string]string{"error": "could not read selected account"})
	}
	infos, err := discoverModels(acct)
	if err != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "model discovery unavailable; refresh credentials or try again"})
	}
	rows := make([]map[string]any, 0, len(infos))
	seen := map[string]bool{}
	modelTests.Lock()
	state := modelTests.accounts[index]
	for _, info := range infos {
		if info.ID == "" {
			continue
		}
		key := strings.ToLower(upstreamModelName(info.ID))
		seen[key] = true
		row := map[string]any{"id": info.ID, "name": info.Name, "enabled": !modelDisabled(storage, info.ID), "benefit": info.Benefit, "context_length": info.ContextWindow, "max_completion_tokens": info.MaxTokens}
		if state != nil {
			if result, ok := state.results[key]; ok {
				row["test"] = result
			}
		}
		rows = append(rows, row)
	}
	modelTests.Unlock()
	for _, id := range disabledModels(storage) {
		key := strings.ToLower(upstreamModelName(id))
		if !seen[key] {
			rows = append(rows, map[string]any{"id": id, "name": id, "enabled": false, "discovered": false})
			seen[key] = true
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["id"].(string) < rows[j]["id"].(string) })
	return managementJSON(http.StatusOK, map[string]any{"auth_index": index, "models": rows, "fetched_at": time.Now().UTC()})
}

func validManagedModel(model string) bool {
	return model != "" && len(model) <= 256 && !strings.ContainsFunc(model, unicode.IsControl)
}

func managementModelEnabled(index, model string, enabled bool) []byte {
	model = upstreamModelName(strings.TrimSpace(model))
	if !validManagedModel(model) {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "a valid model is required"})
	}
	file, _, _, status := managementModelAccount(index)
	if status != http.StatusOK {
		return managementJSON(status, map[string]string{"error": "could not read selected account"})
	}
	unlock := lockAccount(file.Name)
	defer unlock()
	_, storage, err := accountStorage(file)
	if err != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "could not read selected account"})
	}
	disabled := make([]string, 0)
	for _, id := range disabledModels(storage) {
		if !strings.EqualFold(upstreamModelName(id), model) {
			disabled = append(disabled, id)
		}
	}
	if !enabled {
		disabled = append(disabled, model)
	}
	sort.Strings(disabled)
	updated, err := mergeAccountFields(storage, map[string]any{"disabled_models": disabled})
	if err != nil {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid account document"})
	}
	if _, err := hostAuthSave(map[string]any{"name": file.Name, "json": json.RawMessage(updated)}); err != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "could not save model policy"})
	}
	return managementJSON(http.StatusOK, map[string]any{"ok": true, "auth_index": index, "model": model, "enabled": enabled})
}

func managementModelTest(index, model string) []byte {
	model = upstreamModelName(strings.TrimSpace(model))
	if !validManagedModel(model) {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "a valid model is required"})
	}
	file, _, storage, status := managementModelAccount(index)
	if status != http.StatusOK {
		return managementJSON(status, map[string]string{"error": "could not read selected account"})
	}
	if _, disabled := authMetadata(storage); disabled {
		return managementJSON(http.StatusConflict, map[string]string{"error": "account is disabled"})
	}
	if modelDisabled(storage, model) {
		return managementJSON(http.StatusConflict, map[string]string{"error": "model is disabled for this account"})
	}
	// A single in-flight probe per account bounds both spend and upstream load.
	modelTests.Lock()
	state := modelTests.accounts[index]
	if state == nil {
		state = &modelTestState{results: map[string]modelTestResult{}}
		modelTests.accounts[index] = state
	}
	if state.running || time.Since(state.last) < 2*time.Second {
		modelTests.Unlock()
		return managementJSON(http.StatusTooManyRequests, map[string]string{"error": "another model test is running or cooling down"})
	}
	state.running = true
	state.last = time.Now()
	modelTests.Unlock()
	defer func() { modelTests.Lock(); state.running = false; modelTests.Unlock() }()
	started := time.Now()
	request := executorRequest{AuthID: file.ID, Model: model, StorageJSON: storage, Payload: []byte(`{"messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":16}`)}
	raw, _ := json.Marshal(request)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	response, err := executeWithContext(ctx, raw)
	result := modelTestResult{OK: err == nil, CheckedAt: time.Now().UTC(), LatencyMS: time.Since(started).Milliseconds(), Message: "request completed"}
	if err != nil {
		result.Diagnostic = failureDetails(err)
		result.Message = result.Diagnostic.Message
	} else {
		payload, unwrapErr := hostUnwrap(response)
		var envelope struct {
			Payload []byte `json:"payload"`
		}
		var completion struct {
			Choices []struct {
				Message struct {
					Content   string            `json:"content"`
					Reasoning string            `json:"reasoning_content"`
					Tools     []json.RawMessage `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if unwrapErr != nil || json.Unmarshal(payload, &envelope) != nil || json.Unmarshal(envelope.Payload, &completion) != nil || len(completion.Choices) == 0 {
			result.OK = false
			result.Message = "model returned an invalid completion"
		} else if msg := completion.Choices[0].Message; msg.Content == "" && msg.Reasoning == "" && len(msg.Tools) == 0 {
			result.OK = false
			result.Message = "model returned an empty completion"
		}
	}
	modelTests.Lock()
	state.results[strings.ToLower(model)] = result
	modelTests.Unlock()
	return managementJSON(http.StatusOK, result)
}
