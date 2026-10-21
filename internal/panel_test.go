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
		"失败原因", "上游 HTTP", "业务码", "旧记录无详细原因，请重新测试", "未知失败原因",
	} {
		if !strings.Contains(dictionary, `"`+label+`"`) {
			t.Fatalf("PANEL_EN has no translation for %q", label)
		}
	}
}

// TestPanelModelsContract executes the real inline handlers with a minimal DOM.
// No backend model helpers are needed: the transport contract is the boundary.
func TestPanelModelsContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := panelScript(t)
	boot := strings.Index(script, "/* ---------- boot ---------- */")
	if boot < 0 {
		t.Fatal("panel boot marker missing")
	}
	harness := `
const assert=require("node:assert/strict");
class Element {
  constructor(tag="div"){this.tagName=tag;this.children=[];this.style={};this.dataset={};this.disabled=false;this._text="";this.attrs={};this.classes=new Set();this.classList={add:x=>this.classes.add(x),remove:x=>this.classes.delete(x),toggle:(x,on)=>on?this.classes.add(x):this.classes.delete(x),contains:x=>this.classes.has(x)}}
  set textContent(v){this._text=String(v??"");this.children=[]}
  get textContent(){return this._text+this.children.map(x=>typeof x==="string"?x:x.textContent).join("")}
  set innerHTML(v){throw new Error("dynamic HTML is not allowed in this harness")}
  append(...xs){this.children.push(...xs)}
  appendChild(x){this.append(x)}
  replaceChildren(...xs){this._text="";this.children=xs}
  setAttribute(k,v){this.attrs[k]=v}
  getAttribute(k){return this.attrs[k]}
  addEventListener(){}
  remove(){}
  get lastElementChild(){return this.children.at(-1)}
}
const elements=new Map();
const document={documentElement:new Element(),addEventListener(){},getElementById:id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id)},createElement:tag=>new Element(tag),querySelectorAll:selector=>selector===".modal-mask.on"?[...elements.values()].filter(el=>el.classList.contains("on")):[],hidden:false};
const storage=new Map();
const sessionStorage={getItem:k=>storage.get(k)||null,setItem:(k,v)=>storage.set(k,v),removeItem:k=>storage.delete(k)};
const localStorage={getItem:()=>null,setItem:()=>{}};
const window={location:{search:"",host:"localhost",pathname:"/panel"}};window.self=window;window.top=window;
const navigator={userAgent:"test"},history={replaceState(){}};
const setTimeout=()=>0,clearTimeout=()=>{};
let fetch=async()=>{throw new Error("unexpected network request")};
`
	checks := `
(async()=>{
  const deferred=()=>{let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b});return {promise,resolve,reject}};
  const hostile='<img src=x onerror=alert(1)>';
  accounts=[{auth_index:"a",nickname:hostile},{auth_index:"b",nickname:"B"},{auth_index:"disabled",disabled:true}];
  let reads=[],posts=[];
  const catalogue=index=>({auth_index:index,fetched_at:"2026-10-21T00:00:00Z",models:[
    {id:"one",name:hostile,enabled:true,benefit:hostile,context_length:100,max_completion_tokens:10},
    {id:"two",name:"Second",enabled:false}
  ]});
  const read=async(op,index)=>{reads.push([op,index]);return op==="accounts"?{results:accounts}:op==="models"?catalogue(index):{results:[{stats:{},usage:{today:{},records:[]}}]}};
  setPanelTransport(read,async(path,data)=>{posts.push([path,data]);return path==="/models/test"?{ok:false,checked_at:"2026-10-21T01:00:00Z",latency_ms:0,message:hostile,diagnostic:{category:"rate_limited",message:"Rate limited",http_status:200,upstream_code:"RATE_LIMITED"}}:{ok:true,enabled:data.enabled}});
  renderAccounts();applyLanguage();await refresh(false);
  // Diagnostics use the same text-only formatter in usage and model results.
  const diagnostic={category:"rate_limited",message:"Rate limited",http_status:200,upstream_code:"RATE_LIMITED"};
  consoleState.usage={records:[
    {model:"new",failed:true,status:502,diagnostic,estimated:true,latency_ms:9},
    {model:"old",failed:true,status:401},
    {model:"ok",failed:false,status:200},
    {model:"hostile",failed:true,diagnostic:{category:hostile,message:hostile,http_status:hostile,upstream_code:hostile}},
    {model:"local",failed:true,status:503,diagnostic:{category:"network",message:"Network failure"}}
  ],days:[{day:"2026-10-21",requests:5,failed:4}]};
  const usageRows=()=>document.getElementById("usageBody").children[0].children[1].children;
  for(const language of ["zh-CN","en"]){
    panelLanguage=language;applyLanguage();
    const rows=usageRows(),text=document.getElementById("usageBody").textContent;
    assert.equal(rows[0].children.length,8);
    assert(text.includes(tr("失败原因")));
    assert.equal(rows[0].children[7].textContent,language==="en"
      ?"Rate limited · Upstream HTTP: 200 · Business code: RATE_LIMITED"
      :"请求限流 · 上游 HTTP: 200 · 业务码: RATE_LIMITED");
    assert(rows[0].children[7].textContent.includes("200"),"HTTP 200 business errors retain real HTTP status");
    assert(!rows[0].children[7].textContent.includes("502"),"mapped ledger status must not appear as upstream HTTP");
    assert(rows[0].children[6].textContent.includes(tr("失败")),"HTTP 200 must not imply success");
    assert(rows[0].children[6].textContent.includes(tr("估算")),"estimate chip stays in status cell");
    assert.equal(rows[1].children[7].textContent,tr("旧记录无详细原因，请重新测试"));
    assert(!rows[1].children[7].textContent.includes("401"),"do not manufacture historical upstream HTTP");
    assert.equal(rows[2].children[7].textContent,"—","success has no failure reason");
    assert.equal(rows[3].children[7].textContent,tr("未知失败原因")+" · "+tr("上游 HTTP")+": "+hostile+" · "+tr("业务码")+": "+hostile);
    assert.equal(rows[3].children[7].children.length,0,"hostile diagnostic fields remain textContent");
    assert.equal(rows[4].children[7].textContent,tr("网络连接失败"),"local errors have no upstream HTTP");
    assert.equal(diagnosticText({category:"__proto__",message:hostile}),tr("未知失败原因"));
    for(const label of DIAGNOSTIC_LABELS.values())assert(PANEL_EN[label],"diagnostic categories must be bilingual");
    setUsageView("days");
    assert.equal(usageRows()[0].children.length,6,"daily table has no diagnostic column");
    assert(!document.getElementById("usageBody").textContent.includes(tr("失败原因")));
    setUsageView("records");
  }
  panelLanguage="zh-CN";
  assert.equal(reads.filter(x=>x[0]==="models").length,0,"background refresh must not discover models");
  await showModels("a");
  assert.deepEqual(reads.filter(x=>x[0]==="models"),[["models","a"]]);
  assert(document.getElementById("modelsBody").textContent.includes(hostile),"untrusted names remain text");
  assert(document.getElementById("modelsBody").textContent.includes("未测试"));
  assert(!document.getElementById("modelsBody").textContent.includes("实测成功"));
  panelLanguage="en";applyLanguage();
  assert(document.getElementById("modelsBody").textContent.includes("Not tested"));
  assert.equal(reads.filter(x=>x[0]==="models").length,1,"language changes must not discover");
  panelLanguage="zh-CN";
  await testModel("a","two");assert.equal(posts.length,0,"disabled model must not test");
  await setModelEnabled("a","two",true);
  assert.deepEqual(posts[0],["/models/enabled",{auth_index:"a",model:"two",enabled:true}]);
  await testModel("a","one");
  assert.deepEqual(posts[1],["/models/test",{auth_index:"a",model:"one"}]);
  assert.equal(modelState("a").models[0].test.ok,false,"HTTP 200 ok:false is a test failure");
  assert.equal(modelState("a").models[0].enabled,true,"test failure never disables a model");
  assert(document.getElementById("modelsBody").textContent.includes("0 ms"));
  const modelDetail=()=>document.getElementById("modelsBody").children[0].children[1].children[0].children[4].children[1];
  for(const language of ["zh-CN","en"]){
    panelLanguage=language;renderModels();
    assert(modelDetail().textContent.includes(diagnosticText(diagnostic)),"POST test diagnostics render in either language");
    assert(!modelDetail().textContent.includes(hostile),"diagnostic takes priority over generic test message");
    assert(modelDetail().textContent.includes(fmtDate("2026-10-21T01:00:00Z")),"timestamp retained");
  }
  const testResult=modelState("a").models[0].test;
  testResult.diagnostic={category:"unknown",message:hostile,upstream_code:hostile};renderModels();
  assert(modelDetail().textContent.includes(hostile));assert.equal(modelDetail().children.length,0);
  delete testResult.diagnostic;
  for(const message of [hostile,"empty response","ok"]){
    testResult.message=message;testResult.ok=message==="ok";renderModels();
    assert(modelDetail().textContent.includes(message),"legacy/success/empty-response test messages are preserved");
    assert.equal(modelDetail().children.length,0);
    assert(!modelDetail().textContent.includes("Business code"));
  }
  testResult.ok=false;testResult.message="";renderModels();
  assert(modelDetail().textContent.includes(tr("旧记录无详细原因，请重新测试")));
  // Cached discovery results follow the same rendering path as POST results.
  testResult.diagnostic=diagnostic;renderModels();
  assert(modelDetail().textContent.includes(diagnosticText(diagnostic)));
  panelLanguage="zh-CN";renderModels();
  assert.deepEqual(reads.slice(-2),[["stats","a"],["usage","a"]],"test refreshes usage for that account");
  await showModels("disabled");await testModel("disabled","one");assert.equal(posts.length,2,"disabled account must not test");
  const allButtons=el=>[...(el.tagName==="button"?[el]:[]),...el.children.filter(x=>typeof x!=="string").flatMap(allButtons)];
  assert(allButtons(document.getElementById("modelsBody")).filter(b=>b.textContent==="测试").every(b=>b.disabled));
  const pending=deferred();
  setPanelTransport(read,(path,data)=>{posts.push([path,data]);return pending.promise});
  await showModels("a");
  const testing=testModel("a","one"),count=posts.length,discoveryCount=reads.filter(x=>x[0]==="models").length;
  await testModel("a","two");await setModelEnabled("a","two",true);await refreshModels();
  assert.equal(posts.length,count,"per-account busy guard blocks duplicate tests and writes");
  assert.equal(reads.filter(x=>x[0]==="models").length,discoveryCount);
  await showModels("b");consoleState.index="b";consoleState.usage={marker:"b"};
  pending.resolve({ok:true,checked_at:"2026-10-21T02:00:00Z",latency_ms:12,message:hostile});await testing;
  assert.equal(modelsIndex,"b");assert.equal(modelState("b").models[0].test,undefined,"late test cannot update another account");
  assert.equal(consoleState.usage.marker,"b","late usage cannot update another console");
  assert.deepEqual(reads.slice(-2),[["stats","a"],["usage","a"]]);
  const late=deferred();setPanelTransport((op,index)=>op==="models"&&index==="a"?late.promise:read(op,index),null);
  const opening=showModels("a");await showModels("b");closeModal("modelsModal");
  late.resolve(catalogue("a"));await opening;
  assert.equal(modelsIndex,"b");assert(!document.getElementById("modelsModal").classList.contains("on"),"completion must not reopen a closed modal");
  // An older console refresh must not overwrite a newer one, even for the same account.
  consoleState.index="b";
  const stale=deferred();
  setPanelTransport((op,index)=>op==="usage"?stale.promise:read(op,index),null);
  const oldConsole=refreshConsole(false);
  setPanelTransport(async(op,index)=>op==="usage"?{results:[{usage:{marker:"new"}}]}:read(op,index),null);
  await refreshConsole(false);
  stale.resolve({results:[{usage:{marker:"old"}}]});await oldConsole;
  assert.equal(consoleState.usage.marker,"new");
  // Discovery failure preserves the cached catalogue and never launches tests.
  const cached=modelState("a").models;
  setPanelTransport(async()=>{throw new Error("discovery offline")},null);
  await showModels("a");
  assert.equal(modelState("a").models,cached);assert.equal(modelState("a").error,"discovery offline");
  setPanelTransport(read,async()=>{throw new Error("offline")});
  await showModels("a");await testModel("a","one");
  assert.equal(modelState("a").models[0].enabled,true);assert.equal(modelState("a").busy,false);
  assert.equal(modelState("a").error,"offline");
  setPanelTransport(read,null);sessionStorage.setItem(SS_KEY,"wrong");openModal("modelsModal");
  fetch=async(url,opts)=>{assert.equal(url,API_BASE+"/models/test");assert.equal(opts.method,"POST");assert.equal(opts.headers.Authorization,"Bearer wrong");assert.deepEqual(JSON.parse(opts.body),{auth_index:"a",model:"one"});return {status:401}};
  await testModel("a","one");
  assert.equal(document.getElementById("authBox").style.display,"block","401 uses existing auth prompt");
  assert.equal(sessionStorage.getItem(SS_KEY),null);
  assert(!document.getElementById("modelsModal").classList.contains("on"),"auth prompt must not stay behind a modal");
  await load(false);
  assert.equal(document.getElementById("authBox").style.display,"block","background reads must not hide required authentication");
  fetch=async(url,opts)=>{assert.equal(url,API_BASE+"/models/enabled");assert.equal(opts.method,"POST");return {status:403,ok:false,text:async()=>JSON.stringify({error:"forbidden"})}};
  await setModelEnabled("a","one",false);
  assert.equal(modelState("a").models[0].enabled,true,"failed policy write preserves enabled state");
  assert.equal(modelState("a").error,"forbidden");
  // Explicit refresh/open always discovers again, even with a populated cache.
  setPanelTransport(read,async()=>({ok:true,enabled:false}));
  const before=reads.filter(x=>x[0]==="models").length;
  await refreshModels();await showModels("a");
  assert.equal(reads.filter(x=>x[0]==="models").length,before+2);
  await setModelEnabled("a","one",false);assert.equal(modelState("a").models[0].enabled,false);
  const previous=posts.length;await testModel("a","one");assert.equal(posts.length,previous);
  accounts=accounts.filter(a=>a.auth_index!=="a");await testModel("a","two");
  assert.equal(posts.length,previous,"removed account must not test");
})().catch(e=>{console.error(e);process.exitCode=1});
`
	file := filepath.Join(t.TempDir(), "panel-models-contract.js")
	if err := os.WriteFile(file, []byte(harness+script[:boot]+checks), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(node, file).CombinedOutput()
	if err != nil {
		t.Fatalf("model panel contract failed: %v\n%s", err, output)
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
