package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type pluginModelInfo = pluginapi.ModelInfo

var modelAliases = struct {
	sync.RWMutex
	byName map[string]string
}{byName: make(map[string]string)}

func cacheModelAliases(host pluginapi.HostConfigSummary) {
	aliases := make(map[string]string)
	for _, item := range host.OAuthModelAlias[providerName] {
		if item.Alias != "" && item.Name != "" {
			aliases[item.Alias] = item.Name
		}
	}
	modelAliases.Lock()
	modelAliases.byName = aliases
	modelAliases.Unlock()
}

func upstreamModelName(name string) string {
	modelAliases.RLock()
	upstreamName := modelAliases.byName[name]
	if upstreamName == "" {
		upstreamName = modelAliases.byName[strings.TrimPrefix(name, providerName+"/")]
	}
	modelAliases.RUnlock()
	if upstreamName != "" {
		return upstreamName
	}
	return strings.TrimPrefix(name, providerName+"/")
}

// Static models are only a cold-start catalog; account-scoped entitlements come
// from the three upstream discovery sources in FetchModels.
var staticModelIDs = []string{
	"GLM-5.3", "GLM-5.2", "glm-5.2-sft-harmony", "Qwen3-VL-235B",
	"deepseek-v4-flash-0731", "deepseek-v4-pro-0813", "glm-5.3-flash",
}

func modelInfo(info upstream.ModelInfo) pluginModelInfo {
	name := info.Name
	if name == "" {
		name = info.ID
	}
	return pluginModelInfo{
		ID: info.ID, Name: name, ContextLength: info.ContextWindow,
		MaxCompletionTokens: info.MaxTokens, OwnedBy: providerName,
		SupportedGenerationMethods: []string{"chat"},
	}
}

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("invalid model.static request JSON: %w", err)
		}
		cacheModelAliases(req.Host)
	}
	models := make([]pluginModelInfo, 0, len(staticModelIDs))
	for _, id := range staticModelIDs {
		models = append(models, modelInfo(upstream.ModelInfo{ID: id, Name: id}))
	}
	return okEnvelope(map[string]any{"provider": providerName, "models": models}), nil
}

var discoverModels = func(acct *Auth) ([]upstream.ModelInfo, error) {
	return newUpstreamClient(30 * time.Second).FetchModels(acct)
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	if req.AuthID != "" {
		file, err := findAccountByID(req.AuthID)
		if err != nil {
			return nil, err
		}
		_, storage, err := accountStorage(file)
		if err != nil {
			return nil, err
		}
		req.StorageJSON = storage
	}
	acct, err := authFromStorage(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	infos, err := discoverModels(acct)
	if err != nil {
		return nil, err
	}
	models := make([]pluginModelInfo, 0, len(infos))
	for _, info := range infos {
		if info.ID != "" && !modelDisabled(req.StorageJSON, info.ID) {
			models = append(models, modelInfo(info))
		}
	}
	return okEnvelope(map[string]any{"provider": providerName, "models": models}), nil
}
