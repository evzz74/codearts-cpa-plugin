package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"
)

// newTestLedger 返回指向临时目录的账本，不碰用户真实数据。
func newTestLedger(t *testing.T) *usageLedger {
	t.Helper()
	dir := t.TempDir()
	l := &usageLedger{dir: dir, now: func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.Local) }}
	l.devices = map[string]*usageLedgerAccount{}
	l.loaded = true
	return l
}

// TestLedgerRollsUpToday covers the numbers the panel shows today: request
// count, failures and input/output tokens by day and by model.
func TestLedgerRollsUpToday(t *testing.T) {
	l := newTestLedger(t)
	now := l.now()
	l.record(usageEntry{At: now.Add(-time.Hour), Model: "glm-5.2", Input: 100, Output: 20, Total: 120})
	l.record(usageEntry{At: now.Add(-2 * time.Hour), Model: "glm-5.2", Input: 50, Output: 5, Total: 55})
	l.record(usageEntry{At: now.Add(-3 * time.Hour), Model: "deepseek-v3", Input: 10, Output: 1, Total: 11, Failed: true, Status: 429})
	l.record(usageEntry{At: now.AddDate(0, 0, -1), Model: "glm-5.2", Input: 1, Output: 1, Total: 2})

	today, days, models, records, allTime := l.snapshot("default", 50, 62)
	if today.Requests != 3 || today.Failed != 1 {
		t.Fatalf("today = %+v", today)
	}
	if today.Total != 186 || today.Input != 160 || today.Output != 26 {
		t.Fatalf("today tokens = %+v", today)
	}
	if len(days) != 2 || days[0].Day != "2026-10-18" || days[1].Day != "2026-10-17" {
		t.Fatalf("days = %+v", days)
	}
	if len(models) != 2 || models[0].Model != "glm-5.2" || models[0].Total != 177 {
		t.Fatalf("models = %+v", models)
	}
	if len(records) != 4 {
		t.Fatalf("records = %d", len(records))
	}
	// 明细按时间升序存放，快照时倒序输出：最新的一条在最前。
	if !records[0].At.Equal(now.Add(-time.Hour)) {
		t.Fatalf("records not newest-first: %+v", records[0])
	}
	if allTime != 188 {
		t.Fatalf("allTime = %d", allTime)
	}
}

// TestLedgerPersistsAcrossReload 账本必须落盘：宿主重启后今日用量不能归零。
func TestLedgerPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	first := &usageLedger{dir: dir, now: time.Now}
	first.devices = map[string]*usageLedgerAccount{}
	first.record(usageEntry{Device: "device-1", Model: "glm-5.2", Input: 7, Output: 3, Total: 10})

	second := &usageLedger{dir: dir, now: time.Now}
	second.setDevice(t, dir)
	today, _, _, records, _ := second.snapshot("device-1", 50, 62)
	if today.Total != 10 || len(records) != 1 {
		t.Fatalf("reload lost the ledger: today=%+v records=%d", today, len(records))
	}
}

func (l *usageLedger) setDevice(t *testing.T, dir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ledgerFileName))
	if err != nil {
		t.Fatalf("ledger file missing: %v", err)
	}
	var file usageLedgerFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("ledger file is not json: %v", err)
	}
	l.devices = file.Devices
	l.loaded = true
}

// TestLedgerPruneKeepsDayTotals: 超出明细上限的旧记录会被并入日汇总，
// 今日总数不能因此变小。
func TestLedgerPruneKeepsDayTotals(t *testing.T) {
	l := newTestLedger(t)
	base := l.now()
	for i := 0; i < ledgerRecordLimit+50; i++ {
		l.record(usageEntry{At: base.Add(-time.Duration(i) * time.Minute), Model: "glm-5.2", Total: 1})
	}
	today, _, _, records, allTime := l.snapshot("default", 500, 62)
	if len(records) != ledgerRecordLimit {
		t.Fatalf("records = %d, want %d", len(records), ledgerRecordLimit)
	}
	if today.Total != int64(ledgerRecordLimit+50) {
		t.Fatalf("today total = %d, want %d", today.Total, ledgerRecordLimit+50)
	}
	if allTime != today.Total {
		t.Fatalf("allTime = %d, today = %d", allTime, today.Total)
	}
}

// TestLedgerSeparatesDevices keeps two accounts' counters apart.
func TestLedgerSeparatesDevices(t *testing.T) {
	l := newTestLedger(t)
	l.record(usageEntry{Device: "a", Total: 5})
	l.record(usageEntry{Device: "b", Total: 9})
	if today, _, _, _, _ := l.snapshot("a", 10, 62); today.Total != 5 {
		t.Fatalf("device a = %+v", today)
	}
	if today, _, _, _, _ := l.snapshot("b", 10, 62); today.Total != 9 {
		t.Fatalf("device b = %+v", today)
	}
	if today, _, _, _, _ := l.snapshot("missing", 10, 62); today.Total != 0 {
		t.Fatalf("unknown device = %+v", today)
	}
}

// TestLedgerDeviceKeyIsPathSafe makes sure an account id can never turn into a
// path fragment (the key is used as a JSON key, but keep it boring anyway).
func TestLedgerDeviceKeyIsPathSafe(t *testing.T) {
	if got := ledgerDeviceKey("../etc/passwd"); got == "../etc/passwd" {
		t.Fatalf("device key kept traversal: %q", got)
	}
	if got := ledgerDeviceKey(""); got != "default" {
		t.Fatalf("empty device key = %q", got)
	}
}

// TestEstimateFromTextIsSane pins the estimation口径: CJK counts per rune,
// latin per ~4 characters.
func TestEstimateFromTextIsSane(t *testing.T) {
	if got := estimateFromText("你好世界"); got != 4 {
		t.Fatalf("cjk estimate = %d, want 4", got)
	}
	if got := estimateFromText("abcdefgh"); got != 2 {
		t.Fatalf("latin estimate = %d, want 2", got)
	}
	if got := estimateFromText(""); got != 0 {
		t.Fatalf("empty estimate = %d", got)
	}
}

// TestUsageCountsFromRaw reads both OpenAI-style and total-only usage frames.
func TestUsageCountsFromRaw(t *testing.T) {
	in, out, reason, total, ok := usageCountsFromRaw(map[string]any{"prompt_tokens": float64(11), "completion_tokens": float64(7), "total_tokens": float64(18)})
	if !ok || in != 11 || out != 7 || total != 18 || reason != 0 {
		t.Fatalf("openai usage = %d/%d/%d/%d ok=%v", in, out, reason, total, ok)
	}
	in, out, _, total, ok = usageCountsFromRaw(map[string]any{"input_tokens": float64(5), "output_tokens": float64(6)})
	if !ok || in != 5 || out != 6 || total != 11 {
		t.Fatalf("input/output usage = %d/%d/%d ok=%v", in, out, total, ok)
	}
	if _, _, _, _, ok := usageCountsFromRaw(nil); ok {
		t.Fatal("nil usage must not report counts")
	}
}

// stubAccounts installs one readable CodeArts account so the management routes
// can run without a host.
func stubAccounts(t *testing.T) {
	t.Helper()
	oldList, oldGet := hostAuthList, hostAuthGet
	t.Cleanup(func() { hostAuthList, hostAuthGet = oldList, oldGet })
	hostAuthList = func() (json.RawMessage, error) {
		return json.Marshal(map[string]any{"files": []map[string]any{{"auth_index": "idx-1", "name": "codearts-account1.json"}}})
	}
	hostAuthGet = func(string) (json.RawMessage, error) {
		return json.Marshal(map[string]any{
			"auth_index": "idx-1",
			"name":       "codearts-account1.json",
			"json":       base64.StdEncoding.EncodeToString([]byte(`{"type":"codearts","user_id":"account1","user_name":"tester","cloud_dragon_token":"tok","access_key_id":"ak","secret_access_key":"sk"}`)),
		})
	}
}

// TestStatsActionServesConsolePayload checks /stats exposes the same numbers the
// console shows: package name, cycle and the credit donut inputs.
func TestStatsActionServesConsolePayload(t *testing.T) {
	stubAccounts(t)
	oldFetch := fetchStatistics
	t.Cleanup(func() { fetchStatistics = oldFetch })
	fetchStatistics = func(context.Context, *Auth) (upstream.AccountStatistics, error) {
		return upstream.AccountStatistics{
			Package: "体验版", SpecCode: "codearts.agent.individual.trial", IsCredit: true,
			PeriodStart: "2026-10-17", PeriodEnd: "2026-11-18",
			Credit:       &upstream.CreditBalance{Total: 6500, Used: 3.42, Remain: 6496.58, Percent: 0.05},
			Packages:     []upstream.CreditPackage{{Name: "总积分包", Metric: "usageTotalPackageCredit", Total: 6500, Used: 3.42, Remain: 6496.58, Percent: 0.05}},
			PeriodTokens: 41938,
		}, nil
	}
	status, value := responseBody(t, managementBootstrap("/call", valuesWith("op", "stats")))
	if status != 200 {
		t.Fatalf("stats status = %d", status)
	}
	rows, _ := value["results"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", value)
	}
	row, _ := rows[0].(map[string]any)
	stats, _ := row["stats"].(map[string]any)
	if stats == nil {
		t.Fatalf("stats missing: %v", row)
	}
	if stats["package"] != "体验版" || stats["period_end"] != "2026-11-18" {
		t.Fatalf("stats = %v", stats)
	}
	credit, _ := stats["credit"].(map[string]any)
	if credit == nil || credit["total"] != float64(6500) || credit["percent"] != 0.05 {
		t.Fatalf("credit = %v", stats["credit"])
	}
	// 凭据绝不出现在只读代理里。
	joined, _ := json.Marshal(value)
	for _, forbidden := range []string{"secret_access_key", "access_key_id"} {
		if containsString(string(joined), forbidden) {
			t.Fatalf("stats leaked %q", forbidden)
		}
	}
}

// TestUsageActionReadsLedger checks /usage returns today's counters without
// touching the network.
func TestUsageActionReadsLedger(t *testing.T) {
	stubAccounts(t)
	oldLedger := ledger
	t.Cleanup(func() { ledger = oldLedger })
	ledger = newTestLedger(t)
	ledger.record(usageEntry{Device: "idx-1", Model: "glm-5.2", Input: 40, Output: 2, Total: 42})

	query := valuesWith("op", "usage")
	query.Set("auth_index", "idx-1")
	status, value := responseBody(t, managementBootstrap("/call", query))
	if status != 200 {
		t.Fatalf("usage status = %d", status)
	}
	rows, _ := value["results"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", value)
	}
	row, _ := rows[0].(map[string]any)
	usage, _ := row["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("usage missing: %v", row)
	}
	today, _ := usage["today"].(map[string]any)
	if today["total"] != float64(42) || today["requests"] != float64(1) {
		t.Fatalf("today = %v", today)
	}
	models, _ := usage["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models = %v", usage["models"])
	}
}

// TestStatsUnavailableKeepsRowReadable: a failed upstream call must degrade to a
// message, not break the whole account row.
func TestStatsUnavailableKeepsRowReadable(t *testing.T) {
	stubAccounts(t)
	oldFetch := fetchStatistics
	t.Cleanup(func() { fetchStatistics = oldFetch })
	fetchStatistics = func(context.Context, *Auth) (upstream.AccountStatistics, error) {
		return upstream.AccountStatistics{}, errFake
	}
	status, value := responseBody(t, managementBootstrap("/call", valuesWith("op", "stats")))
	if status != 200 {
		t.Fatalf("stats status = %d", status)
	}
	rows, _ := value["results"].([]any)
	row, _ := rows[0].(map[string]any)
	if row["ok"] != false || row["message"] == nil {
		t.Fatalf("row = %v", row)
	}
}

type fakeError struct{}

func (fakeError) Error() string { return "upstream down" }

var errFake = fakeError{}

// TestUsageHookRecordsHostRecords covers the host-driven path (usage.handle):
// records from other providers are ignored, CodeArts records are counted once.
func TestUsageHookRecordsHostRecords(t *testing.T) {
	oldLedger := ledger
	t.Cleanup(func() { ledger = oldLedger })
	ledger = newTestLedger(t)

	other, _ := json.Marshal(map[string]any{"Provider": "claude", "Model": "x", "Detail": map[string]any{"TotalTokens": 100}})
	handleUsageRecord(other)
	if today, _, _, _, _ := ledger.snapshot("default", 10, 62); today.Total != 0 {
		t.Fatalf("foreign provider was counted: %+v", today)
	}

	own, _ := json.Marshal(map[string]any{
		"Provider": "codearts", "Model": "glm-5.2", "AuthIndex": "idx-1",
		"Detail": map[string]any{"InputTokens": 30, "OutputTokens": 12, "TotalTokens": 42},
	})
	handleUsageRecord(own)
	today, _, _, records, _ := ledger.snapshot("idx-1", 10, 62)
	if today.Total != 42 || len(records) != 1 {
		t.Fatalf("host record not counted: %+v / %d records", today, len(records))
	}

	// 空记录（无 token 也无失败）不该进入账本。
	handleUsageRecord([]byte(`{"Provider":"codearts","Model":"glm-5.2"}`))
	if _, _, _, records, _ := ledger.snapshot("idx-1", 10, 62); len(records) != 1 {
		t.Fatalf("zero-token record was stored: %d records", len(records))
	}
}
