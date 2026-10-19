package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type retestProvider struct {
	complete func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
}

func (p retestProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.complete(ctx, req)
}
func (p retestProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, errors.New("test expects non-streaming"))
	}
}

func newRetestServer(t *testing.T) (*Server, int64) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("test postgres not configured")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	td := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{ctx: ctx, m: &Manager{pg: pg, dir: td, tasks: map[string]*Task{}},
		chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{}}
	if err := s.seedFindingRetester(); err != nil {
		t.Fatal(err)
	}
	fid, err := pg.AddFinding(0, 0, "retest-server", "复测测试", "high", "summary", "original proof", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldAugment, oldResolve, oldPrompt := agent.ToolAugment, agent.ToolResolve, agent.PromptOverride
	agent.ToolAugment = func(context.Context, string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
		return s.findingRetestTools(), agent.DeferredInfo{}, func() {}
	}
	agent.ToolResolve = nil
	agent.PromptOverride = func(string) (string, bool) { return agent.RetesterDefaultPrompt, true }
	t.Cleanup(func() {
		cancel()
		waitRetestIdle(t, s)
		agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = oldAugment, oldResolve, oldPrompt
		_, _ = pg.Exec(`DELETE FROM conversations WHERE id IN (SELECT conversation_id FROM finding_retests WHERE finding_id=$1)`, fid)
		_, _ = pg.DeleteFinding(fid)
		pg.Close()
	})
	return s, fid
}

func setRetestProvider(s *Server, p retestProvider) {
	s.chatAgent = agent.NewChatAgent(p, "test", s.m.dir, nil, 10000)
	s.chatAgent.SetNonStreaming(func() bool { return true })
}

func retestRequest(handler http.HandlerFunc, method string, id int64, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/", strings.NewReader(body))
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	// 发现接口现在按部门可见性收敛（requireFindingRead/Write）：这些用例校验的是复测
	// 流程本身，所以按管理员（不受限）调用，与改造前的行为一致。
	r = r.WithContext(context.WithValue(r.Context(), authCtxKey{}, &db.User{ID: 1, IsAdmin: true}))
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func waitRetestIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.chatMu.Lock()
		n := len(s.chatBusy)
		s.chatMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("conversation did not become idle")
}

func TestRetestActiveStatusLifecycle(t *testing.T) {
	s, fid := newRetestServer(t)
	check := func(want string, conversationID int64) {
		t.Helper()
		w := retestRequest(s.listActiveFindingRetests, http.MethodGet, 0, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var body struct {
			Retests []db.ActiveFindingRetest `json:"retests"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		var matches []db.ActiveFindingRetest
		for _, item := range body.Retests {
			if item.FindingID == fid {
				matches = append(matches, item)
			}
		}
		if want == "" {
			if len(matches) != 0 {
				t.Fatalf("terminal retest still active: %+v", matches)
			}
		} else if len(matches) != 1 || matches[0].Status != want || matches[0].ConversationID != conversationID {
			t.Fatalf("active=%+v want status=%s conversation=%d", matches, want, conversationID)
		}
		for _, field := range []string{`"snapshot"`, `"evidence"`, `"notes"`, `"summary"`} {
			if strings.Contains(w.Body.String(), field) {
				t.Fatalf("status response leaks %s", field)
			}
		}
	}
	check("", 0)
	for _, terminal := range []string{"completed", "failed", "stopped"} {
		r, c, _, err := s.m.pg.CreateFindingRetest(t.Context(), fid, "private notes")
		if err != nil {
			t.Fatal(err)
		}
		check("pending", c.ID)
		if _, err := s.m.pg.StartFindingRetest(t.Context(), r.ID); err != nil {
			t.Fatal(err)
		}
		check("running", c.ID)
		if terminal == "completed" {
			if err := s.m.pg.RecordFindingRetestResult(t.Context(), c.ID, "confirmed", "summary", "proof"); err != nil {
				t.Fatal(err)
			}
			check("running", c.ID) // A staged verdict is still running until the turn ends.
		}
		if err := s.m.pg.FinishFindingRetest(r.ID, terminal, ""); err != nil {
			t.Fatal(err)
		}
		check("", 0)
	}
}

func TestRetestHTTPThroughConversationAndTools(t *testing.T) {
	s, fid := newRetestServer(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	setRetestProvider(s, retestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return llm.Message{}, "", llm.Usage{}, ctx.Err()
			}
		}
		name, input := "get_finding_retest_context", `{}`
		if n == 2 {
			serialized, _ := json.Marshal(req.Messages)
			if !bytes.Contains(serialized, []byte("original proof")) {
				t.Error("agent did not receive source evidence")
			}
			name, input = "record_finding_retest_result", `{"verdict":"inconclusive","summary":"缺少测试登录态","evidence":"已检查原证据；当前缺少有效登录态，无法确认修复状态。"}`
		}
		if n > 2 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("复测结论已保存")}}, "end_turn", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: strconv.Itoa(int(n)), Name: name, Input: json.RawMessage(input)}}}, "tool_use", llm.Usage{}, nil
	}})
	w := retestRequest(s.startFindingRetest, "POST", fid, `{"notes":"仅验证原接口"}`)
	if w.Code != 202 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider not called")
	}
	dup := retestRequest(s.startFindingRetest, "POST", fid, `{}`)
	if dup.Code != 200 || !strings.Contains(dup.Body.String(), `"created":false`) {
		t.Fatalf("duplicate %d %s", dup.Code, dup.Body)
	}
	close(release)
	waitRetestIdle(t, s)
	rows, err := s.m.pg.ListFindingRetests(fid)
	if err != nil || len(rows) != 1 || rows[0].Status != "completed" || rows[0].Verdict != "inconclusive" {
		t.Fatalf("history=%+v err=%v", rows, err)
	}
	if calls.Load() != 3 {
		t.Fatal("unexpected LLM calls", calls.Load())
	}
	f, _ := s.m.pg.GetFinding(fid)
	if f.Status != db.FindingPending || f.Evidence != "original proof" {
		t.Fatal("source modified")
	}
	listing := retestRequest(s.listFindingRetests, "GET", fid, "")
	if listing.Code != 200 || strings.Contains(listing.Body.String(), `"snapshot"`) {
		t.Fatalf("history %d %s", listing.Code, listing.Body)
	}
	res, err := s.findingRetestTools()[1].Call(intercept.WithConvID(t.Context(), *rows[0].ConversationID), json.RawMessage(`{"verdict":"confirmed","summary":"bad overwrite","evidence":"bad"}`), nil)
	if err != nil || !res.IsError {
		t.Fatal("sealed result accepted", res, err)
	}
}

func TestRetestStopsFailuresAndNoVerdict(t *testing.T) {
	for _, mode := range []string{"stop", "failure", "no_verdict"} {
		t.Run(mode, func(t *testing.T) {
			s, fid := newRetestServer(t)
			setRetestProvider(s, retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				if mode == "stop" {
					<-ctx.Done()
					return llm.Message{}, "", llm.Usage{}, ctx.Err()
				}
				if mode == "failure" {
					return llm.Message{}, "", llm.Usage{}, errors.New("test model failed")
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("未保存结论")}}, "end_turn", llm.Usage{}, nil
			}})
			w := retestRequest(s.startFindingRetest, "POST", fid, `{}`)
			if w.Code != 202 {
				t.Fatalf("create %d %s", w.Code, w.Body)
			}
			if mode == "stop" {
				var body struct {
					Retest db.FindingRetest `json:"retest"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				stop := retestRequest(s.pgStopConversation, "POST", *body.Retest.ConversationID, `{}`)
				if stop.Code != 200 || !strings.Contains(stop.Body.String(), "stopping") {
					t.Fatalf("immediate stop: %d %s", stop.Code, stop.Body)
				}
			}
			waitRetestIdle(t, s)
			rows, _ := s.m.pg.ListFindingRetests(fid)
			want := "failed"
			if mode == "stop" {
				want = "stopped"
			}
			if len(rows) != 1 || rows[0].Status != want || rows[0].Error == "" {
				t.Fatalf("expected %s: %+v", want, rows)
			}
		})
	}
}

func TestRetestConfirmedUpdatesFindingThroughConversation(t *testing.T) {
	s, fid := newRetestServer(t)
	var calls atomic.Int32
	setRetestProvider(s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if calls.Add(1) == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "review-result", Name: "record_finding_retest_result", Input: json.RawMessage(`{"verdict":"confirmed","summary":"复核确认漏洞真实","evidence":"回到源码核对 UserController.java:88：触发路径可达、参数可控、拼接直达查询。"}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("结论已保存")}}, "end_turn", llm.Usage{}, nil
	}})
	w := retestRequest(s.startFindingRetest, "POST", fid, `{}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	waitRetestIdle(t, s)
	f, err := s.m.pg.GetFinding(fid)
	if err != nil || f.Status != db.FindingConfirmed || f.Evidence != "original proof" {
		t.Fatalf("confirmed finding=%+v err=%v", f, err)
	}
	// The new manual status is also accepted by the existing update API.
	w = retestRequest(s.patchFinding, "PATCH", fid, `{"status":"fixed"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"fixed"`) {
		t.Fatalf("manual fixed: %d %s", w.Code, w.Body)
	}
}

func TestRetestRefutedMarksFalsePositiveThroughConversation(t *testing.T) {
	s, fid := newRetestServer(t)
	var calls atomic.Int32
	setRetestProvider(s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if calls.Add(1) == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "review-result", Name: "record_finding_retest_result", Input: json.RawMessage(`{"verdict":"refuted","summary":"存在有效校验，漏洞不成立","evidence":"UserController.java:90 有白名单校验，输入不可控。"}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("结论已保存")}}, "end_turn", llm.Usage{}, nil
	}})
	w := retestRequest(s.startFindingRetest, "POST", fid, `{}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	waitRetestIdle(t, s)
	f, err := s.m.pg.GetFinding(fid)
	if err != nil || f.Status != db.FindingFalsePositive || f.Evidence != "original proof" {
		t.Fatalf("refuted finding=%+v err=%v", f, err)
	}
}

func TestRetestValidationSeedingAndScope(t *testing.T) {
	s, fid := newRetestServer(t)
	for _, tc := range []struct {
		id   int64
		body string
		code int
	}{{0, `{}`, 400}, {fid, `{`, 400}, {fid, `{"notes":"` + strings.Repeat("文", 4001) + `"}`, 400}, {999999999, `{}`, 404}, {fid, `{}`, 503}} {
		w := retestRequest(s.startFindingRetest, "POST", tc.id, tc.body)
		if w.Code != tc.code {
			t.Fatalf("code=%d want=%d %s", w.Code, tc.code, w.Body)
		}
	}
	rows, _ := s.m.pg.ListFindingRetests(fid)
	if len(rows) != 0 {
		t.Fatal("invalid request created state")
	}
	for _, tool := range s.findingRetestTools() {
		res, err := tool.Call(t.Context(), json.RawMessage(`{"verdict":"confirmed","summary":"x","evidence":"y"}`), nil)
		if err != nil || !res.IsError {
			t.Fatal("unassociated conversation accepted", tool.Name(), res, err)
		}
	}
	a, _ := s.m.pg.GetAgentByKey(db.FindingRetestAgentKey)
	if a == nil || a.Builtin {
		t.Fatal("missing editable retester")
	}
	var triggers int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM agent_triggers WHERE agent_key=$1`, db.FindingRetestAgentKey).Scan(&triggers); err != nil || triggers != 0 {
		t.Fatal("unexpected automatic trigger", triggers, err)
	}
	if _, err := s.m.pg.SavePrompt(a.ID, "customized retest prompt", "test", "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFindingRetester(); err != nil {
		t.Fatal(err)
	}
	var prompt string
	if err := s.m.pg.QueryRow(`SELECT p.template_text FROM agent_prompts p JOIN agents a ON a.current_prompt_id=p.id WHERE a.key=$1`, a.Key).Scan(&prompt); err != nil || prompt != "customized retest prompt" {
		t.Fatal("seed overwrote prompt", prompt, err)
	}
	_, _ = s.m.pg.SavePrompt(a.ID, agent.RetesterDefaultPrompt, "restore", "test")
}

// migrateRetesterPrompt 把老库里的「渗透复测」提示词一次性换成「静态复核」版。它改写的是
// 用户可见的提示词，所以两件事都要成立：老库真的被换掉；换完之后用户在 Agent 管理里的
// 编辑不会被下一次启动覆盖（否则每次重启都吞掉用户改动）。
//
// 为什么需要这个迁移：seedFindingRetester 只在首次创建 agent 时写提示词，seedPrompts 用的
// SeedPromptIfEmpty 一旦有版本就不再覆盖——老库会一直留着 v1 那段「用原 PoC 做实际请求/
// 命令并记录响应」的提示词，与本平台不发起任何请求的能力直接矛盾。
func TestMigrateRetesterPromptReplacesLegacyPromptOnce(t *testing.T) {
	s, _ := newRetestServer(t)
	a, err := s.m.pg.GetAgentByKey(db.FindingRetestAgentKey)
	if err != nil || a == nil {
		t.Fatalf("retester agent: %+v err=%v", a, err)
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.SavePrompt(a.ID, agent.RetesterDefaultPrompt, "test restore", "test")
		_, _ = s.m.pg.Exec(`UPDATE agents SET name='漏洞复核' WHERE key=$1`, db.FindingRetestAgentKey)
	})

	// 老库状态：提示词是渗透复测版，且没有迁移标记。
	const legacy = "你是授权渗透测试系统的「漏洞复测」Agent，用原 PoC 做实际请求并记录响应。"
	if _, err := s.m.pg.SavePrompt(a.ID, legacy, "legacy", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`UPDATE agents SET name='漏洞复测' WHERE key=$1`, db.FindingRetestAgentKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingRetesterReviewMigrated); err != nil {
		t.Fatal(err)
	}

	if err := s.migrateRetesterPrompt(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if got, err := s.m.pg.CurrentPrompt(a.ID); err != nil || got != agent.RetesterDefaultPrompt {
		t.Fatalf("迁移后提示词未替换: err=%v got=%.80q", err, got)
	}
	if v, ok, err := s.m.pg.GetSetting(settingRetesterReviewMigrated); err != nil || !ok || v != "1" {
		t.Fatalf("迁移标记未落库: v=%q ok=%t err=%v", v, ok, err)
	}
	if renamed, err := s.m.pg.GetAgentByKey(db.FindingRetestAgentKey); err != nil || renamed.Name != "漏洞复核" {
		t.Fatalf("迁移后名字未对齐: %+v err=%v", renamed, err)
	}

	// 用户随后在 Agent 管理里改过提示词：下次启动不能再覆盖。
	const edited = "我自己的复核提示词"
	if _, err := s.m.pg.SavePrompt(a.ID, edited, "user edit", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateRetesterPrompt(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.m.pg.CurrentPrompt(a.ID); err != nil || got != edited {
		t.Fatalf("迁移覆盖了用户编辑: err=%v got=%.40q", err, got)
	}

	// 用户自己起过名字的 agent 不能被迁移改名（清标记是为了强制再跑一次迁移）。
	if _, err := s.m.pg.Exec(`UPDATE agents SET name='我的复核 Agent' WHERE key=$1`, db.FindingRetestAgentKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingRetesterReviewMigrated); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateRetesterPrompt(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.m.pg.GetAgentByKey(db.FindingRetestAgentKey); err != nil || got.Name != "我的复核 Agent" {
		t.Fatalf("迁移覆盖了用户改的名字: %+v err=%v", got, err)
	}
}

// retestTool 按名字取复核会话的工具，缺了就失败——工具被误删/改名要在这里炸，
// 而不是等到线上模型拿不到工具。
func retestTool(t *testing.T, s *Server, name string) actool.CoreTool {
	t.Helper()
	for _, tool := range s.findingRetestTools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("复核工具 %s 不存在", name)
	return nil
}

// TestSaveFindingPacketToolLifecycle 覆盖复核 Agent 的纠错面：报文修订工具只能写
// 本会话关联漏洞、只能写它锚定的那个接口、只在本轮复核进行中可写，且重复保存递增
// revision（人工修订走同一条落库链路）。
func TestSaveFindingPacketToolLifecycle(t *testing.T) {
	s, fid := newRetestServer(t)
	ep, err := s.m.pg.Assets().UpsertEndpoint(db.UpsertEndpointReq{URL: "https://pk.example.com/api/user", Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM assets WHERE id=$1`, ep) })
	if _, err := s.m.pg.Exec(`UPDATE findings SET asset_ids=$2::jsonb WHERE id=$1`, fid, fmt.Sprintf("[%d]", ep)); err != nil {
		t.Fatal(err)
	}

	tool := retestTool(t, s, saveFindingPacketTool)
	raw := "GET /api/user HTTP/1.1\r\nHost: pk.example.com\r\nCookie: <会话 Cookie>\r\n\r\n"
	call := func(ctx context.Context, in map[string]any) actool.Result {
		t.Helper()
		payload, _ := json.Marshal(in)
		res, err := tool.Call(ctx, payload, nil)
		if err != nil {
			t.Fatalf("工具调用出错: %v", err)
		}
		return res
	}

	// 会话未关联复核记录：与另外两个工具一样拒绝，且不写任何东西。
	if res := call(t.Context(), map[string]any{"asset_id": ep, "raw_request": raw}); !res.IsError {
		t.Fatal("未关联复核记录的会话被接受")
	}

	// 建立并启动一轮复核。
	r, conv, created, err := s.m.pg.CreateFindingRetest(t.Context(), fid, "")
	if err != nil || !created {
		t.Fatalf("创建复核失败: created=%t err=%v", created, err)
	}
	if _, err := s.m.pg.StartFindingRetest(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	ctx := intercept.WithConvID(t.Context(), conv.ID)

	// 不属于本漏洞锚定的接口 → 拒绝（挂错接口会让复测打错目标）。
	if res := call(ctx, map[string]any{"asset_id": ep + 100000, "raw_request": raw}); !res.IsError {
		t.Fatal("外来 asset_id 被接受")
	}
	// 空报文 → 拒绝。
	if res := call(ctx, map[string]any{"asset_id": ep, "raw_request": "  "}); !res.IsError {
		t.Fatal("空报文被接受")
	}

	// 正常写入：落库来源是 llm、生成者是复核 agent，首次 revision=1。
	if res := call(ctx, map[string]any{"asset_id": ep, "raw_request": raw,
		"grounded_refs": []string{"src/main/java/com/x/UserController.java:88"}}); res.IsError {
		t.Fatalf("保存报文失败: %s", res.Flatten())
	}
	p, err := s.m.pg.GetFindingPacket(fid, ep)
	if err != nil || p == nil {
		t.Fatalf("报文未落库: p=%+v err=%v", p, err)
	}
	if p.Source != db.RetestPacketSourceLLM || p.Generator != db.FindingRetestAgentKey || p.Revision != 1 || !p.Valid {
		t.Fatalf("报文属性不符: source=%s generator=%s revision=%d valid=%t errors=%v",
			p.Source, p.Generator, p.Revision, p.Valid, p.Validation.Errors)
	}
	if p.RawRequest != raw {
		t.Fatalf("报文正文不符: %q", p.RawRequest)
	}

	// 同一接口再存一次：覆盖并递增 revision，而不是新增一行。
	revised := strings.Replace(raw, "GET /api/user", "GET /api/user?id=1", 1)
	if res := call(ctx, map[string]any{"asset_id": ep, "raw_request": revised}); res.IsError {
		t.Fatalf("修订报文失败: %s", res.Flatten())
	}
	p2, err := s.m.pg.GetFindingPacket(fid, ep)
	if err != nil || p2 == nil || p2.Revision != 2 || p2.RawRequest != revised {
		t.Fatalf("修订未覆盖同一行: p=%+v err=%v", p2, err)
	}
	if list, err := s.m.pg.ListFindingPackets(fid); err != nil || len(list) != 1 {
		t.Fatalf("同一接口应只有一份报文: n=%d err=%v", len(list), err)
	}

	// 复核结束后（结论已封存）不允许再改报文：追问可以解释，不能改产物。
	if err := s.m.pg.RecordFindingRetestResult(t.Context(), conv.ID, db.VerdictConfirmed, "s", "e"); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.FinishFindingRetest(r.ID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if res := call(ctx, map[string]any{"asset_id": ep, "raw_request": raw}); !res.IsError {
		t.Fatal("已结束的复核仍可写报文")
	}
}

// TestSeedFindingRetesterBindsPacketTool 老库升级路径：新工具名会被首插种进 tools 表
// 并绑给 retester，否则模型永远看不到它（SeedTool 是 first-insert-only，改已有工具
// 的 schema 到不了老库，所以这里刻意用新工具名而不是给旧工具加参数）。
func TestSeedFindingRetesterBindsPacketTool(t *testing.T) {
	s, _ := newRetestServer(t)
	tool, err := s.m.pg.GetTool(saveFindingPacketTool)
	if err != nil || tool == nil {
		t.Fatalf("新工具未 seed: %+v err=%v", tool, err)
	}
	if !tool.Enabled || !slices.Contains(tool.Agents, db.FindingRetestAgentKey) {
		t.Fatalf("新工具未启用或未绑定 retester: enabled=%t agents=%v", tool.Enabled, tool.Agents)
	}
}

// TestMigrateRetesterPromptAddsPacketTool 提示词迁移只补「没被用户改过」的正文：
// 老库停在「复核只能读报文」的旧文本上时补上修订说明；用户自定义的提示词不动。
func TestMigrateRetesterPromptAddsPacketTool(t *testing.T) {
	s, _ := newRetestServer(t)
	a, err := s.m.pg.GetAgentByKey(db.FindingRetestAgentKey)
	if err != nil || a == nil {
		t.Fatalf("retester agent: %+v err=%v", a, err)
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.SavePrompt(a.ID, agent.RetesterDefaultPrompt, "test restore", "system")
		_, _ = s.m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingRetesterPacketsMigrated)
	})

	// 老库形态：系统写的旧正文（不含新工具名），且迁移标记未落。
	if _, err := s.m.pg.SavePrompt(a.ID, "旧的复核提示词（还没有报文修订能力）", "内置默认", "system"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingRetesterPacketsMigrated); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateRetesterPromptPackets(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if got, err := s.m.pg.CurrentPrompt(a.ID); err != nil || !strings.Contains(got, saveFindingPacketTool) {
		t.Fatalf("迁移未补上报文修订说明: err=%v got=%.120q", err, got)
	}
	if v, ok, err := s.m.pg.GetSetting(settingRetesterPacketsMigrated); err != nil || !ok || v != "1" {
		t.Fatalf("迁移标记未落库: v=%q ok=%t err=%v", v, ok, err)
	}

	// 用户改过的提示词：清标记强制再跑一次，正文必须原样保留。
	const edited = "我自己的复核提示词"
	if _, err := s.m.pg.SavePrompt(a.ID, edited, "user edit", "ui"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingRetesterPacketsMigrated); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateRetesterPromptPackets(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.m.pg.CurrentPrompt(a.ID); err != nil || got != edited {
		t.Fatalf("迁移覆盖了用户编辑: err=%v got=%.40q", err, got)
	}
}
