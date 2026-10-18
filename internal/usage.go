package main

// 用量账本：本插件自己记账。
//
// 为什么不直接读 CPA：宿主把用量记录推给 usage_plugin 能力（usage.handle RPC），
// 但插件执行器走的链路实测不上报 token（CodeArts 的 SSE 流里没有 usage 帧），
// 且宿主的 redis 队列只保留 60 秒、是 pop 语义，做不了历史。所以真正的
// token 数在这里按请求统计：上游没给 usage 时按字符数估算（与 CodeArts 控制台
// 的 usage_token_num 口径不同，面板会标注来源）。
//
// 账本落盘位置由宿主给出的目录决定，不把用户输入拼进路径：文件名固定为
// usage-ledger.json，写入走临时文件 + rename，避免半截文件。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ledgerFileName = "usage-ledger.json"
	// ledgerRecordLimit 单账户保留的明细条数；面板只展示最近一页，旧记录按
	// 天汇总后仍然可用，所以截断不影响今日统计。
	ledgerRecordLimit = 500
	// ledgerDayLimit 保留的天数（含今天）。
	ledgerDayLimit = 62
)

// usageEntry 一条请求级用量明细。
type usageEntry struct {
	At        time.Time `json:"at"`
	Device    string    `json:"device,omitempty"`
	Account   string    `json:"account,omitempty"`
	Model     string    `json:"model,omitempty"`
	Input     int64     `json:"input"`
	Output    int64     `json:"output"`
	Reasoning int64     `json:"reasoning,omitempty"`
	Total     int64     `json:"total"`
	Estimated bool      `json:"estimated,omitempty"`
	Stream    bool      `json:"stream,omitempty"`
	Failed    bool      `json:"failed,omitempty"`
	Status    int       `json:"status,omitempty"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
}

// usageDay 某一天的汇总。
type usageDay struct {
	Day       string `json:"day"`
	Requests  int64  `json:"requests"`
	Failed    int64  `json:"failed"`
	Input     int64  `json:"input"`
	Output    int64  `json:"output"`
	Reasoning int64  `json:"reasoning"`
	Total     int64  `json:"total"`
}

// usageModel 某模型在本窗口内的汇总。
type usageModel struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Input    int64  `json:"input"`
	Output   int64  `json:"output"`
	Total    int64  `json:"total"`
}

// usageLedgerFile 是落盘结构，按 device_id 分桶。
type usageLedgerFile struct {
	Version int                            `json:"version"`
	Devices map[string]*usageLedgerAccount `json:"devices"`
}

// usageLedgerAccount 单账户账本。
type usageLedgerAccount struct {
	Records []usageEntry               `json:"records"`
	Pruned  map[string]*usagePrunedDay `json:"pruned,omitempty"`
}

// usageLedger 进程内账本。所有方法在锁内完成，调用方不需要同步。
type usageLedger struct {
	mu      sync.Mutex
	dir     string
	devices map[string]*usageLedgerAccount
	loaded  bool
	now     func() time.Time
}

var ledger = &usageLedger{now: time.Now}

// ledgerStorageDir 解析账本目录。
//
// 宿主不给插件数据目录（registration 只有 config_yaml），所以优先用
// CODEARTS_USAGE_DIR 覆盖；否则落在用户配置目录下的 codearts2api 里。
// 目录拿到后只用来拼接**固定文件名**，不拼接任何请求输入。
func ledgerStorageDir() string {
	if override := strings.TrimSpace(os.Getenv("CODEARTS_USAGE_DIR")); override != "" {
		return override
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "codearts2api")
}

// initLedgerStorage 绑定账本目录；重复调用（每次 register/reconfigure）不会
// 重复载盘，只有目录真的变了才重新载入。
func initLedgerStorage() { setLedgerDir(ledgerStorageDir()) }

// setLedgerDir 绑定落盘目录并载入已有账本。宿主每次启动/重配置都会给出目录，
// 目录变化时重新载入，避免把旧数据写到新位置。
func setLedgerDir(dir string) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.dir == dir && ledger.loaded {
		return
	}
	ledger.dir = dir
	ledger.devices = map[string]*usageLedgerAccount{}
	ledger.loaded = true
	ledger.loadLocked()
}

func (l *usageLedger) loadLocked() {
	if l.dir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(l.dir, ledgerFileName))
	if err != nil {
		return
	}
	var file usageLedgerFile
	if json.Unmarshal(raw, &file) != nil || file.Devices == nil {
		return
	}
	for device, account := range file.Devices {
		if account == nil {
			continue
		}
		l.devices[device] = account
	}
}

func (l *usageLedger) persistLocked() {
	if l.dir == "" {
		return
	}
	file := usageLedgerFile{Version: 1, Devices: l.devices}
	raw, err := json.Marshal(file)
	if err != nil {
		return
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(l.dir, "usage-ledger-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	// Windows 上 rename 到已存在的文件会失败，先删再换；极端情况下丢掉的是
	// 上一次的账本，而不是本次新数据。
	_ = os.Remove(filepath.Join(l.dir, ledgerFileName))
	if err := os.Rename(name, filepath.Join(l.dir, ledgerFileName)); err != nil {
		os.Remove(name)
	}
}

// record 追加一条用量。device 为空时归到 "default" 桶。
func (l *usageLedger) record(entry usageEntry) {
	if entry.Total <= 0 && !entry.Failed {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	device := ledgerDeviceKey(entry.Device)
	account := l.devices[device]
	if account == nil {
		account = &usageLedgerAccount{}
		l.devices[device] = account
	}
	if entry.At.IsZero() {
		entry.At = l.now()
	}
	entry.At = entry.At.Local()
	account.Records = append(account.Records, entry)
	// 落盘只保留最近 N 条。为了让「今日/按天/按模型」汇总与明细一致，被截掉的
	// 旧记录先并入日汇总，快照时再加回来。
	if extra := len(account.Records) - ledgerRecordLimit; extra > 0 {
		pruned := append([]usageEntry(nil), account.Records[:extra]...)
		account.Records = append([]usageEntry(nil), account.Records[extra:]...)
		account.mergePruned(pruned)
	}
	l.persistLocked()
}

// usagePrunedDay 是被截断记录留下的按天汇总。
type usagePrunedDay struct {
	Day       string `json:"day"`
	Requests  int64  `json:"requests"`
	Failed    int64  `json:"failed"`
	Input     int64  `json:"input"`
	Output    int64  `json:"output"`
	Reasoning int64  `json:"reasoning"`
	Total     int64  `json:"total"`
}

// mergePruned 把被截断的记录累加进按天汇总，并丢掉超出窗口的旧天。
func (a *usageLedgerAccount) mergePruned(entries []usageEntry) {
	if a.Pruned == nil {
		a.Pruned = make(map[string]*usagePrunedDay)
	}
	for _, entry := range entries {
		key := dayKey(entry.At)
		day := a.Pruned[key]
		if day == nil {
			day = &usagePrunedDay{Day: key}
			a.Pruned[key] = day
		}
		day.Requests++
		if entry.Failed {
			day.Failed++
		}
		day.Input += entry.Input
		day.Output += entry.Output
		day.Reasoning += entry.Reasoning
		day.Total += entry.Total
	}
	cutoff := dayKey(time.Now().AddDate(0, 0, -ledgerDayLimit))
	for key := range a.Pruned {
		if key < cutoff {
			delete(a.Pruned, key)
		}
	}
}

// dayKey 本地时区的日期键，与控制台的「今日」一致。
func dayKey(t time.Time) string { return t.Local().Format("2006-01-02") }

// snapshot 生成某账户在窗口内的汇总、明细与今日数据。
//
// since 之前的明细不参与展示，但按天/模型汇总仍覆盖窗口内全部记录。
func (l *usageLedger) snapshot(device string, limit int, days int) (usageDay, []usageDay, []usageModel, []usageEntry, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	account := l.devices[ledgerDeviceKey(device)]
	if account == nil {
		return usageDay{Day: dayKey(l.now())}, nil, nil, nil, 0
	}
	if limit <= 0 {
		limit = 50
	}
	today := dayKey(l.now())
	daysOf := make(map[string]*usageDay)
	modelOf := make(map[string]*usageModel)
	// 先放被截断的按天汇总，再用明细累加，两边都不会双计。
	for key, pruned := range account.Pruned {
		if pruned == nil {
			continue
		}
		day := &usageDay{Day: key, Requests: pruned.Requests, Failed: pruned.Failed, Input: pruned.Input, Output: pruned.Output, Reasoning: pruned.Reasoning, Total: pruned.Total}
		daysOf[key] = day
	}
	for _, entry := range account.Records {
		key := dayKey(entry.At)
		day := daysOf[key]
		if day == nil {
			day = &usageDay{Day: key}
			daysOf[key] = day
		}
		day.Requests++
		if entry.Failed {
			day.Failed++
		}
		day.Input += entry.Input
		day.Output += entry.Output
		day.Reasoning += entry.Reasoning
		day.Total += entry.Total
		name := entry.Model
		if name == "" {
			name = "unknown"
		}
		model := modelOf[name]
		if model == nil {
			model = &usageModel{Model: name}
			modelOf[name] = model
		}
		model.Requests++
		model.Input += entry.Input
		model.Output += entry.Output
		model.Total += entry.Total
	}
	// 明细按时间倒序，取前 limit 条。账本里追加顺序未必等于时间顺序
	// （并发请求、宿主补推的记录），所以先按时间排序再截取。
	ordered := append([]usageEntry(nil), account.Records...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].At.After(ordered[j].At) })
	records := make([]usageEntry, 0, len(ordered))
	for _, entry := range ordered {
		if len(records) >= limit {
			break
		}
		records = append(records, entry)
	}
	daily := make([]usageDay, 0, len(daysOf))
	for _, day := range daysOf {
		daily = append(daily, *day)
	}
	sort.Slice(daily, func(i, j int) bool { return daily[i].Day > daily[j].Day })
	if days > 0 && len(daily) > days {
		daily = daily[:days]
	}
	models := make([]usageModel, 0, len(modelOf))
	for _, model := range modelOf {
		models = append(models, *model)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Total == models[j].Total {
			return models[i].Model < models[j].Model
		}
		return models[i].Total > models[j].Total
	})
	todayStat := usageDay{Day: today}
	if day, ok := daysOf[today]; ok {
		todayStat = *day
	}
	var allTime int64
	for _, day := range daysOf {
		allTime += day.Total
	}
	return todayStat, daily, models, records, allTime
}

// ledgerDeviceKey 把账户标识规整成桶名。只允许出现文件名安全的字符，
// 因为桶名会作为 JSON key 落盘（不会直接拼路径，仍然是纵深防御）。
func ledgerDeviceKey(device string) string {
	device = strings.TrimSpace(device)
	if device == "" {
		return "default"
	}
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, device)
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "default"
	}
	return cleaned
}

// estimateFromText 在上游不给 usage 时估算 token。
//
// 口径：中日韩字符按 1 token/字，其余按 4 字符/token（与主流 BPE 经验值一致）。
// 这只是下界估算，面板必须标注，不能当成官方用量。
func estimateFromText(text string) int64 {
	if text == "" {
		return 0
	}
	var cjk, other int64
	for _, r := range text {
		switch {
		case r >= 0x2E80 && r <= 0x9FFF, r >= 0xAC00 && r <= 0xD7AF, r >= 0xF900 && r <= 0xFAFF, r >= 0xFF00 && r <= 0xFFEF:
			cjk++
		default:
			other++
		}
	}
	tokens := cjk + (other+3)/4
	if tokens <= 0 {
		return 1
	}
	return tokens
}

// usageCountsFromRaw 把上游 usage 帧换算成输入/输出/推理 token。
//
// CodeArts 的 usage 与 OpenAI 同形（prompt_tokens/completion_tokens），
// 但不同模型可能给 total_tokens 或 input_tokens/output_tokens，两种都认。
func usageCountsFromRaw(raw map[string]any) (input, output, reasoning, total int64, ok bool) {
	if raw == nil {
		return 0, 0, 0, 0, false
	}
	num := func(keys ...string) (int64, bool) {
		for _, key := range keys {
			v, exists := raw[key]
			if !exists {
				continue
			}
			switch typed := v.(type) {
			case float64:
				if typed != 0 {
					return int64(typed), true
				}
			case int64:
				if typed != 0 {
					return typed, true
				}
			case int:
				if typed != 0 {
					return int64(typed), true
				}
			case json.Number:
				if n, err := typed.Int64(); err == nil && n != 0 {
					return n, true
				}
			}
		}
		return 0, false
	}
	input, okIn := num("prompt_tokens", "input_tokens")
	output, okOut := num("completion_tokens", "output_tokens")
	if details, isMap := raw["completion_tokens_details"].(map[string]any); isMap {
		if n, isNum := details["reasoning_tokens"].(float64); isNum && n > 0 {
			reasoning = int64(n)
		}
	}
	if reasoning == 0 {
		reasoning, _ = num("reasoning_tokens")
	}
	total, okTotal := num("total_tokens")
	if !okTotal {
		if !okIn && !okOut {
			return 0, 0, 0, 0, false
		}
		total = input + output + reasoning
	}
	if total <= 0 {
		return 0, 0, 0, 0, false
	}
	return input, output, reasoning, total, true
}

// recordUsage 是执行器调用的记账入口，失败只影响统计，绝不影响请求。
func recordUsage(entry usageEntry) {
	defer func() { _ = recover() }()
	ledger.record(entry)
}

// usageLedgerSnapshot 供管理接口读取。
func usageLedgerSnapshot(device string, limit int) map[string]any {
	today, daily, models, records, allTime := ledger.snapshot(device, limit, ledgerDayLimit)
	return map[string]any{
		"today":       today,
		"days":        daily,
		"models":      models,
		"records":     records,
		"all_time":    allTime,
		"record_keep": ledgerRecordLimit,
		"day_keep":    ledgerDayLimit,
		"timezone":    time.Now().Format("MST"),
	}
}
