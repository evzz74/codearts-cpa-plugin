package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// EpPackageStatistics 账户/套餐与积分统计（官方码道 IDE 的 AccountInfoPane /
// PackageInfoService 读它，逆向自 out/main.js:54259）。
//
// 响应是**裸对象**（没有 code/data 信封），这点与 /v1/ops/* 不同，
// 因此不能复用 creditRequest 的信封解析。
const EpPackageStatistics = "/snap-manager/v1/statistics/plugin"

// CreditMetricLabels 积分口径 → 展示名。usageTotalPackageCredit 是总额，
// 其余三项是构成明细（基础/按需/赠送），相加会重复计算。
var CreditMetricLabels = map[string]string{
	"usageTotalPackageCredit":    "总积分包",
	"usageBasicPackageCredit":    "基础积分包",
	"usageOnDemandPackageCredit": "按需积分包",
	"usageBonusPackageCredit":    "赠送积分包",
}

const totalCreditMetric = "usageTotalPackageCredit"

// CreditPackage 一个积分包口径（对应 metrics[] 的一条）。
type CreditPackage struct {
	Name    string  `json:"name"`
	Metric  string  `json:"metric"`
	Total   float64 `json:"total"`   // package_credit_amount
	Used    float64 `json:"used"`    // package_credit_used
	Remain  float64 `json:"remain"`  // package_credit_remain
	Percent float64 `json:"percent"` // 已用百分比（0-100，total<=0 时为 0）
}

// modelMetric 统计周期内的模型用量。
type modelMetric struct {
	ID     string `json:"model_id"`
	Name   string `json:"model_name"`
	Type   string `json:"model_type"`
	Desc   string `json:"model_desc"`
	Used   int64  `json:"usage_token_num"`
	Quota  int64  `json:"model_quota_num"` // -1 = 不限量
	Enable bool   `json:"enable"`
	Status string `json:"status"`
}

// PackageInfo 套餐概要。
type packageInfo struct {
	SpecCode string `json:"spec_code"`
	Status   string `json:"status"`
	NameCN   string `json:"package_name_cn"`
	NameEN   string `json:"package_name_en"`
	URL      string `json:"package_url"`
	IsCredit bool   `json:"is_credit_package"`
	IsToken  bool   `json:"is_token_package"`
}

// AccountStatistics statistics/plugin 的脱敏解析结果。
//
// Credit 为 nil 表示该账户没有积分口径（Token 计费账户），与「有口径但为 0」
// 严格区分：前者不能画扇形图，后者是 0%。
type AccountStatistics struct {
	Package     string          `json:"package"`
	SpecCode    string          `json:"spec_code"`
	IsCredit    bool            `json:"is_credit_package"`
	IsToken     bool            `json:"is_token_package"`
	PackageURL  string          `json:"package_url,omitempty"`
	PeriodStart string          `json:"period_start,omitempty"` // 统计周期（积分包有效期）
	PeriodEnd   string          `json:"period_end,omitempty"`
	Credit      *CreditBalance  `json:"credit,omitempty"`
	Packages    []CreditPackage `json:"packages,omitempty"`
	// PeriodTokens 统计周期内的聊天 token 用量（metrics[usageTokenChatMessages]）。
	PeriodTokens int64 `json:"period_tokens"`
	// Models 周期内各模型的 token 用量，按用量降序。
	Models []ModelUsage `json:"models,omitempty"`
}

// CreditBalance 积分总量。
type CreditBalance struct {
	Total   float64 `json:"total"`
	Used    float64 `json:"used"`
	Remain  float64 `json:"remain"`
	Percent float64 `json:"percent"` // 已用百分比（0-100）
}

// ModelUsage 单模型周期用量。
type ModelUsage struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Used  int64  `json:"used"`
	Quota int64  `json:"quota"`
}

type statisticMetric struct {
	Name        string  `json:"name"`
	Value       float64 `json:"value"`
	TokenNum    float64 `json:"usage_token_num"`
	TokenAmount float64 `json:"package_token_amount"`
	CreditTotal float64 `json:"package_credit_amount"`
	CreditUsed  float64 `json:"package_credit_used"`
	CreditLeave float64 `json:"package_credit_remain"`
	Show        bool    `json:"show"`
}

type statisticsResponse struct {
	Metrics []statisticMetric `json:"metrics"`
	Models  []modelMetric     `json:"model_metrics"`
	Start   string            `json:"start_date"`
	End     string            `json:"end_date"`
	Package packageInfo       `json:"package"`
}

// FetchStatistics 查询账户套餐与积分统计（只读）。
func (c *Client) FetchStatistics(ctx context.Context, cred SignCredential) (AccountStatistics, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.snapURL(EpPackageStatistics), nil)
	if err != nil {
		return AccountStatistics{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	signRequest(req, nil, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return AccountStatistics{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return AccountStatistics{}, err
	}
	if resp.StatusCode >= 400 {
		return AccountStatistics{}, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Path: EpPackageStatistics, Message: truncateStr(string(raw), 300)}
	}
	return ParseStatistics(raw)
}

// ParseStatistics 解析 statistics/plugin 响应。也接受 {code,data} 信封形态，
// 以免上游某天改成信封后整页空白。
func ParseStatistics(raw []byte) (AccountStatistics, error) {
	body := raw
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Code != nil {
		if *envelope.Code != 0 {
			return AccountStatistics{}, fmt.Errorf("statistics business code %d", *envelope.Code)
		}
		if len(envelope.Data) > 0 {
			body = envelope.Data
		}
	}
	var payload statisticsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return AccountStatistics{}, fmt.Errorf("parse statistics: %w", err)
	}
	out := AccountStatistics{
		Package:     firstNonEmpty(payload.Package.NameCN, payload.Package.NameEN),
		SpecCode:    payload.Package.SpecCode,
		IsCredit:    payload.Package.IsCredit,
		IsToken:     payload.Package.IsToken,
		PackageURL:  payload.Package.URL,
		PeriodStart: payload.Start,
		PeriodEnd:   payload.End,
	}
	var totalRemain, totalUsed, totalAmount float64
	var sawTotal bool
	for _, metric := range payload.Metrics {
		switch metric.Name {
		case totalCreditMetric:
			sawTotal = true
			totalAmount, totalUsed, totalRemain = metric.CreditTotal, metric.CreditUsed, metric.CreditLeave
		case "usageTokenChatMessages":
			out.PeriodTokens = int64(metric.TokenNum)
		}
		label, isCredit := CreditMetricLabels[metric.Name]
		if !isCredit {
			continue
		}
		// 额度为 0 的分类不列为资源包：列出来只会让「N 个积分包」虚高。
		if metric.CreditTotal <= 0 && metric.CreditLeave <= 0 {
			continue
		}
		out.Packages = append(out.Packages, CreditPackage{
			Name: label, Metric: metric.Name,
			Total: round2(metric.CreditTotal), Used: round2(metric.CreditUsed), Remain: round2(metric.CreditLeave),
			Percent: percentOf(metric.CreditUsed, metric.CreditTotal),
		})
	}
	if sawTotal || len(out.Packages) > 0 {
		if !sawTotal {
			// 总额口径缺失时回退为分类求和（不加总 usageTotalPackageCredit 本身）。
			totalAmount, totalUsed, totalRemain = 0, 0, 0
			for _, pkg := range out.Packages {
				if pkg.Metric == totalCreditMetric {
					continue
				}
				totalAmount += pkg.Total
				totalUsed += pkg.Used
				totalRemain += pkg.Remain
			}
		}
		out.Credit = &CreditBalance{
			Total: round2(totalAmount), Used: round2(totalUsed), Remain: round2(totalRemain),
			Percent: percentOf(totalUsed, totalAmount),
		}
	}
	for _, model := range payload.Models {
		if model.Used <= 0 {
			continue
		}
		out.Models = append(out.Models, ModelUsage{ID: model.ID, Name: firstNonEmpty(model.Name, model.ID), Used: model.Used, Quota: model.Quota})
	}
	sortModelUsage(out.Models)
	return out, nil
}

func sortModelUsage(models []ModelUsage) {
	for i := 1; i < len(models); i++ {
		for j := i; j > 0 && models[j].Used > models[j-1].Used; j-- {
			models[j], models[j-1] = models[j-1], models[j]
		}
	}
}

func percentOf(used, total float64) float64 {
	if total <= 0 {
		return 0
	}
	p := used / total * 100
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return round2(p)
}

func round2(v float64) float64 {
	if v != v { // NaN
		return 0
	}
	return float64(int64(v*100+signOf(v)*0.5)) / 100
}

func signOf(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
