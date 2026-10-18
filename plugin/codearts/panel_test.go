package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The panel is plain HTML+JS served from the plugin binary, so a compile of the
// Go side cannot catch a broken script. These tests check the panel source
// directly (no browser needed), which is what a bad edit normally breaks.

// panelScript returns the inline <script> body of the embedded panel.
func panelScript(t *testing.T) string {
	t.Helper()
	start := strings.Index(codeartsPanel, "<script>")
	end := strings.LastIndex(codeartsPanel, "</script>")
	if start < 0 || end <= start {
		t.Fatal("panel has no inline script")
	}
	return codeartsPanel[start+len("<script>") : end]
}

// TestPanelScriptParses runs the panel script through node --check when node is
// available. Skipped elsewhere so the suite still runs on a bare Go toolchain.
func TestPanelScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "panel.js")
	if err := os.WriteFile(file, []byte(panelScript(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "--check", file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("panel script does not parse: %v\n%s", err, stderr.String())
	}
}

// TestPanelDeclaresConsoleOverview pins the three sections the console mirror
// needs: the credit donut, today's tokens and the usage records table.
func TestPanelDeclaresConsoleOverview(t *testing.T) {
	for _, marker := range []string{
		`id="creditDonut"`, `id="creditArc"`, `id="creditPct"`,
		`id="creditTotal"`, `id="creditUsed"`, `id="creditRemain"`,
		`id="creditPackages"`, `id="creditPeriod"`,
		`id="tokenTotal"`, `id="tokenRequests"`, `id="tokenInput"`, `id="tokenOutput"`,
		`id="tokenSpark"`, `id="tokenModels"`, `id="tokenDay"`,
		`id="usageBody"`, `data-view="records"`, `data-view="days"`,
		`id="consoleAccount"`,
		// 扇形图靠 pathLength=100 让 dasharray 直接等于百分比。
		`pathLength="100"`,
	} {
		if !strings.Contains(codeartsPanel, marker) {
			t.Fatalf("panel is missing %s", marker)
		}
	}
}

// TestPanelConsoleReadsBackendRoutes keeps the panel wired to the routes the
// plugin actually registers.
func TestPanelConsoleReadsBackendRoutes(t *testing.T) {
	script := panelScript(t)
	for _, marker := range []string{
		`panelReadCall("stats"`, `panelReadCall("usage"`, `panelReadCall("accounts"`,
		`panelReadCall("credits"`,
	} {
		if !strings.Contains(script, marker) {
			t.Fatalf("panel does not call %s", marker)
		}
	}
	if strings.Contains(script, `readCall("stats"`) {
		t.Fatal("panel must use the injectable transport, not readCall directly")
	}
	// 每次 register/reconfigure 都会重新渲染，语言切换也必须重绘概览。
	if !strings.Contains(script, "renderConsole()") {
		t.Fatal("panel never renders the console overview")
	}
}

// TestPanelI18nCoversConsoleLabels guards against adding console text without a
// translation: every Chinese label used in data-i18n must exist in PANEL_EN.
func TestPanelI18nCoversConsoleLabels(t *testing.T) {
	raw := codeartsPanel
	startMarker := "const PANEL_EN"
	start := strings.Index(raw, startMarker)
	if start < 0 {
		t.Fatal("PANEL_EN missing")
	}
	start += len(startMarker)
	end := strings.Index(raw[start:], "\n};")
	if end < 0 {
		t.Fatal("PANEL_EN is not terminated")
	}
	dictionary := raw[start : start+end]
	for _, label := range []string{
		"积分用量", "今日 tokens 消耗", "用量记录", "已用", "总积分", "剩余",
		"总 tokens", "请求数", "输入", "输出", "明细", "按天", "时间", "模型", "耗时", "状态",
		"估算", "暂无用量记录", "今日暂无请求", "统计周期",
	} {
		if !strings.Contains(dictionary, `"`+label+`"`) {
			t.Fatalf("PANEL_EN has no translation for %q", label)
		}
	}
}

// TestUsageStatsPayloadShape documents the JSON the panel consumes, so a backend
// rename breaks this test instead of the UI.
func TestUsageStatsPayloadShape(t *testing.T) {
	raw, err := json.Marshal(usageLedgerSnapshot("idx-1", 10))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"today", "days", "models", "records", "all_time", "record_keep", "day_keep"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("usage payload lost %q: %s", key, raw)
		}
	}
	today, _ := payload["today"].(map[string]any)
	for _, key := range []string{"day", "requests", "failed", "input", "output", "total"} {
		if _, ok := today[key]; !ok {
			t.Fatalf("today lost %q: %s", key, raw)
		}
	}
}
