package upstream

import (
	"testing"
)

// creditStatisticsBody mirrors the live statistics/plugin response: a bare
// object (no code/data envelope) whose metrics carry both credit and token
// counters. Values are the shape the console reads.
const creditStatisticsBody = `{
  "metrics": [
    {"name":"usageBasicPackageCredit","package_credit_amount":500,"package_credit_used":500,"package_credit_remain":0},
    {"name":"usageBonusPackageCredit","package_credit_amount":6000,"package_credit_used":3.42,"package_credit_remain":5996.58},
    {"name":"usageOnDemandPackageCredit","package_credit_amount":0,"package_credit_used":0,"package_credit_remain":0},
    {"name":"usageTotalPackageCredit","package_credit_amount":6500,"package_credit_used":3.42,"package_credit_remain":6496.58},
    {"name":"usageTokenChatMessages","usage_token_num":41938,"package_token_amount":1}
  ],
  "model_metrics": [
    {"model_id":"glm-5.2","model_name":"GLM-5.2","usage_token_num":30000,"model_quota_num":-1},
    {"model_id":"deepseek-v3","model_name":"DeepSeek-V3","usage_token_num":11938,"model_quota_num":-1},
    {"model_id":"idle","model_name":"Idle","usage_token_num":0}
  ],
  "start_date":"2026-10-17",
  "end_date":"2026-11-18",
  "package":{"spec_code":"codearts.agent.individual.trial","package_name_cn":"体验版","is_credit_package":true,"is_token_package":false},
  "show":true
}`

// TestParseStatisticsUsesTotalMetric pins the credit口径: the total must come
// from usageTotalPackageCredit, never from summing the category metrics.
func TestParseStatisticsUsesTotalMetric(t *testing.T) {
	stats, err := ParseStatistics([]byte(creditStatisticsBody))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credit == nil {
		t.Fatal("credit balance missing")
	}
	if stats.Credit.Total != 6500 || stats.Credit.Used != 3.42 || stats.Credit.Remain != 6496.58 {
		t.Fatalf("credit = %+v, want total 6500 used 3.42 remain 6496.58", *stats.Credit)
	}
	// 3.42/6500 = 0.0526% → rounds to 0.05.
	if stats.Credit.Percent != 0.05 {
		t.Fatalf("percent = %v, want 0.05", stats.Credit.Percent)
	}
	if stats.Package != "体验版" || stats.SpecCode != "codearts.agent.individual.trial" || !stats.IsCredit {
		t.Fatalf("package = %+v", stats)
	}
	if stats.PeriodStart != "2026-10-17" || stats.PeriodEnd != "2026-11-18" {
		t.Fatalf("period = %s..%s", stats.PeriodStart, stats.PeriodEnd)
	}
	if stats.PeriodTokens != 41938 {
		t.Fatalf("period tokens = %d", stats.PeriodTokens)
	}
}

// TestParseStatisticsSkipsEmptyPackagesAndSortsModels asserts the panel never
// shows empty resource packages and reads models by descending usage.
func TestParseStatisticsSkipsEmptyPackagesAndSortsModels(t *testing.T) {
	stats, err := ParseStatistics([]byte(creditStatisticsBody))
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range stats.Packages {
		if pkg.Metric == "usageOnDemandPackageCredit" {
			t.Fatal("empty on-demand package must not be listed")
		}
	}
	// total + basic + bonus survive; on-demand（全 0）is dropped.
	if len(stats.Packages) != 3 {
		t.Fatalf("packages = %+v", stats.Packages)
	}
	if len(stats.Models) != 2 {
		t.Fatalf("models = %+v", stats.Models)
	}
	if stats.Models[0].ID != "glm-5.2" || stats.Models[1].ID != "deepseek-v3" {
		t.Fatalf("models not sorted by usage: %+v", stats.Models)
	}
}

// TestParseStatisticsAcceptsEnvelope keeps the parser alive if the upstream ever
// wraps the bare object in {code,data}.
func TestParseStatisticsAcceptsEnvelope(t *testing.T) {
	raw := []byte(`{"code":0,"message":"ok","data":{"metrics":[{"name":"usageTotalPackageCredit","package_credit_amount":100,"package_credit_used":25,"package_credit_remain":75}]}}`)
	stats, err := ParseStatistics(raw)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credit == nil || stats.Credit.Total != 100 || stats.Credit.Percent != 25 {
		t.Fatalf("credit = %+v", stats.Credit)
	}
}

// TestParseStatisticsTokenPackageHasNoDonut: a token-only package must report a
// nil credit balance so the panel can say "no credit package" instead of 0%.
func TestParseStatisticsTokenPackageHasNoDonut(t *testing.T) {
	raw := []byte(`{"metrics":[{"name":"usageTokenChatMessages","usage_token_num":900,"package_token_amount":100000}],"package":{"is_token_package":true,"package_name_cn":"Token 包"}}`)
	stats, err := ParseStatistics(raw)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credit != nil {
		t.Fatalf("token package must not report credits: %+v", *stats.Credit)
	}
	if !stats.IsToken || stats.PeriodTokens != 900 {
		t.Fatalf("token stats = %+v", stats)
	}
}

// TestParseStatisticsBusinessCodeFails: code != 0 in that shape is a failure.
func TestParseStatisticsBusinessCodeFails(t *testing.T) {
	if _, err := ParseStatistics([]byte(`{"code":200000,"message":"denied"}`)); err == nil {
		t.Fatal("business error code must fail")
	}
}
