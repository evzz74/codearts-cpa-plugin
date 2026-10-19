package main

// 宿主的 usage_plugin 回调用量记录（usage.handle）。
//
// 实测插件执行器这条链路不会触发它：CodeArts 的 SSE 里没有 usage 帧，宿主
// 也不会替插件补 usage。所以真正的记账在两个执行器入口（见 executor.go）。
// 这里仍然实现，是为了在宿主确实推记录时（比如以后补上 usage 帧）把真实
// 计数写进账本，而不是只依赖估算。

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleUsageRecord 处理宿主推送的用量记录。
func handleUsageRecord(raw []byte) []byte {
	if len(raw) == 0 {
		return okEnvelope(map[string]any{"ok": true})
	}
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return okEnvelope(map[string]any{"ok": false})
	}
	// 只关心本执行器产生的记录；其它提供方的用量不归 CodeArts 统计。
	if record.Provider != "" && record.Provider != providerName {
		return okEnvelope(map[string]any{"ok": true, "skipped": true})
	}
	// 时间戳留空交给账本补：账本用的是自己的可注入时钟（production 为 time.Now，
	// 测试里可控）。这里若直接取 time.Now()，记录会落进「真实今天」的日期桶，
	// 而快照按账本时钟算「今日」，两者错开就永远读不到这条记录。
	entry := usageEntry{
		At:        record.RequestedAt,
		Device:    ledgerDeviceKey(record.AuthIndex),
		Account:   record.AuthType,
		Model:     record.Model,
		Input:     record.Detail.InputTokens,
		Output:    record.Detail.OutputTokens,
		Reasoning: record.Detail.ReasoningTokens,
		Total:     record.Detail.TotalTokens,
		Failed:    record.Failed,
		Status:    record.Failure.StatusCode,
		LatencyMS: record.Latency.Milliseconds(),
	}
	if entry.Total == 0 {
		entry.Total = entry.Input + entry.Output + entry.Reasoning
	}
	// 没有 token 也没有失败的记录没有统计价值，别让「0 token 请求」刷屏。
	if entry.Total <= 0 && !entry.Failed {
		return okEnvelope(map[string]any{"ok": true, "skipped": true})
	}
	recordUsage(entry)
	return okEnvelope(map[string]any{"ok": true})
}
