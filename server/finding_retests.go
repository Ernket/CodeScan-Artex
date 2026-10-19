package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	actool "github.com/Autumn-27/norma/tool"
)

func (s *Server) listActiveFindingRetests(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	// 部门可见性：进行中的复核也要按「发现 → 任务」收敛，否则任意账号都能看到全平台
	// 正在跑的复核。findingScopeForRequest 内部已 fail-closed（解析失败返回空作用域）。
	scope := s.findingScopeForRequest(r)
	items, err := pg.ListActiveFindingRetests(r.Context(), scope)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"retests": items})
}

func (s *Server) listFindingRetests(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	// 部门可见性：发现的可见范围跟随其所属任务；不可见按不存在处理。
	if !s.requireFindingRead(w, r, id) {
		return
	}
	f, err := pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	items, err := pg.ListFindingRetests(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"retests": items})
}

func (s *Server) startFindingRetest(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	var req struct {
		Notes string `json:"notes"`
	}
	if !decodeConversationRequest(w, r, &req) {
		return
	}
	req.Notes = strings.TrimSpace(req.Notes)
	if utf8.RuneCountInString(req.Notes) > 4000 {
		writeErr(w, 400, "复测补充说明最多 4000 个字符")
		return
	}
	// 部门访客只读：发起复测会写入复测记录并起会话，按写权限校验。
	if !s.requireFindingWrite(w, r, id) {
		return
	}
	f, err := pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	a, err := pg.GetAgentByKey(db.FindingRetestAgentKey)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if a == nil || !a.Enabled {
		writeErr(w, 409, "漏洞复测 Agent 不存在或未启用，请在 Agent 管理中配置 retester")
		return
	}
	for _, key := range []string{"get_finding_retest_context", "record_finding_retest_result"} {
		t, err := pg.GetTool(key)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if t == nil || !t.Enabled || !slices.Contains(t.Agents, a.Key) {
			writeErr(w, 409, "请为复测 Agent 启用并绑定工具："+key)
			return
		}
	}
	if s.resolveChatAgent(&db.Conversation{AgentKey: a.Key}) == nil {
		writeErr(w, 503, s.chatUnavailableReason())
		return
	}
	if s.ctx.Err() != nil {
		writeErr(w, 503, "服务正在停止")
		return
	}
	retest, conv, created, err := pg.CreateFindingRetest(r.Context(), id, req.Notes)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "finding not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if created {
		busyKey := s.convBusyKey(conv.ID)
		s.chatMu.Lock()
		s.chatBusy[busyKey] = true
		s.chatMu.Unlock()
		s.runConversation(conv, retest.InitialMessage(), busyKey)
	}
	code := http.StatusOK
	if created {
		code = http.StatusAccepted
	}
	writeJSON(w, code, map[string]any{"retest": retest, "created": created})
}

// saveFindingPacketTool 是复核 Agent 修订复测报文的工具名。种子、一次性迁移与
// 提示词都按名字引用它，改名要同步这三处。
const saveFindingPacketTool = "save_finding_packet"

// All tools derive the finding from server-owned conversation context. Tool
// arguments cannot redirect a result into a different finding or conversation.
func (s *Server) findingRetestTools() []actool.CoreTool {
	return []actool.CoreTool{
		roTool("get_finding_retest_context", "读取当前复核会话关联的漏洞证据快照（原证据、报告、锚定资产、推演报文）、复核状态、补充说明与当前任务约束。无参数，只能读取本会话。",
			objSchema(map[string]any{}), func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
				r, err := s.m.pg.FindingRetestForConversation(ctx, intercept.ConvIDFromContext(ctx))
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if r == nil {
					return actool.Errorf("当前会话未关联复测记录，请从漏洞详情发起复测"), nil
				}
				var constraints []db.Constraint
				f, err := s.m.pg.GetFinding(r.FindingID)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if f != nil && f.TaskID != nil {
					if task, ok := s.m.Task(strconv.FormatInt(*f.TaskID, 10)); ok {
						constraints, err = task.Store.ListConstraints()
						if err != nil {
							return actool.Errorf(err.Error()), nil
						}
					}
				}
				return jsonResult(map[string]any{"retest": r, "current_constraints": constraints})
			}),
		wrTool("record_finding_retest_result", "为当前复核会话保存唯一结论；原漏洞证据与报告保持不变。会话成功结束且结论为 confirmed 时，系统自动把漏洞状态改为「已确认」；refuted 改为「误报」；inconclusive 保留原状态。必须提供基于源码的复核依据，无法确认时写明阻塞原因。",
			objSchema(map[string]any{
				"verdict":  map[string]any{"type": "string", "enum": []string{"confirmed", "refuted", "inconclusive"}},
				"summary":  strParam("本次复核结论摘要"),
				"evidence": strParam("Markdown：复核过的文件:行号、原证据的逐条核对结果、结论依据；无法确认则列出已检查内容和阻塞原因"),
			}, "verdict", "summary", "evidence"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
				var a struct {
					Verdict  string `json:"verdict"`
					Summary  string `json:"summary"`
					Evidence string `json:"evidence"`
				}
				if err := json.Unmarshal(in, &a); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if err := s.m.pg.RecordFindingRetestResult(ctx, intercept.ConvIDFromContext(ctx), a.Verdict, a.Summary, a.Evidence); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				return jsonResult(map[string]any{"saved": true, "verdict": a.Verdict})
			}),
		// 复核的纠错面：报文是唯一允许复核 Agent 修订的产物。它与结论刻意分成两个工具：
		//  1. 结论工具是「一次定死」的（拒绝第二个结论），而修订必须能反复进行；
		//  2. 报文是**非证据**的复测用例，修订它不等于推翻结论，两者在语义上不该绑在一起。
		// 复用人工修订那条链路（SaveRetestPacket）：同一套校验、同一张表、同样递增 revision，
		// 所以这里不需要任何新的存储或迁移。
		wrTool(saveFindingPacketTool,
			"保存/修订本漏洞【某一个接口】的复测报文（非证据的复测用例）。某个接口还没有报文，或报文的方法/路径/参数与锚定接口的源码不一致时，用它在那个接口下保存一份依据源码推演的报文；同一接口重复保存即覆盖，revision 递增。"+
				"取值必须有源码依据（grounded_refs 用「文件:行号」），无法从源码确定的值写进 assumptions，Cookie / Authorization 等凭证一律写 <占位符>——绝不编造看起来真实的凭证，也不要为了写报文去真正发起请求、连接目标或编译运行代码。"+
				"报文始终是复测用例、不是证据：修订它不改变漏洞结论，也不能替代 record_finding_retest_result 里的证据。服务端会解析校验，未通过也会保存并标记为不可信。",
			objSchema(map[string]any{
				"asset_id": map[string]any{"type": "integer", "description": "这份报文对应哪个接口：传 get_finding_retest_context 快照里 assets 中该接口的 id，必须是本漏洞锚定的接口（对不上的 id 会被拒绝，避免复测打错目标）。"},
				"raw_request": strParam("完整、可直接粘贴复测的 HTTP/1.1 请求报文：起始行 + Host 等头部 + 空行 + 正文。路径与参数必须与 asset_id 指向的那个接口一致，凭证一律 <占位符>。"),
				"raw_response": strParam("可选：同一请求的预期响应报文（同为推演产物，仅作参考）"),
				"grounded_refs": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "每条报文取值的源码依据，格式 文件:行号（例如 src/main/java/com/x/UserController.java:88）"},
				"assumptions": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "无法从源码确定、按假设填写的值（例如会话凭证、运行时生成的 ID）"},
			}, "asset_id", "raw_request"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
				var a struct {
					AssetID      json.RawMessage `json:"asset_id"`
					RawRequest   string          `json:"raw_request"`
					RawResponse  string          `json:"raw_response"`
					GroundedRefs []string        `json:"grounded_refs"`
					Assumptions  []string        `json:"assumptions"`
				}
				if err := json.Unmarshal(in, &a); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				// 归属只认会话：与另外两个工具同一原则，模型传什么都改不了它复核的是哪个漏洞。
				r, err := s.m.pg.FindingRetestForConversation(ctx, intercept.ConvIDFromContext(ctx))
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if r == nil {
					return actool.Errorf("当前会话未关联复核记录，无法修订报文；请从漏洞详情发起复核"), nil
				}
				// 只在本轮复核进行中允许修订：复核结束后会话仍可追问，但被追问时再改报文
				// 会绕过「结论已封存」的语义（record_finding_retest_result 同样拒绝二次结论）。
				if r.Status != "running" {
					return actool.Errorf(fmt.Sprintf("本次复核已结束（状态 %s），报文只能在本轮复核进行中修订；需要再改请从漏洞详情重新发起复核", r.Status)), nil
				}
				if strings.TrimSpace(a.RawRequest) == "" {
					return actool.Errorf("raw_request 不能为空"), nil
				}
				f, err := s.m.pg.GetFinding(r.FindingID)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if f == nil {
					return actool.Errorf("漏洞不存在或已删除"), nil
				}
				assetID := parseProfileID(a.AssetID)
				interfaces, err := s.findingPacketInterfaces(f)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if len(interfaces) == 0 {
					return actool.Errorf("本漏洞没有锚定 HTTP 接口资产，无法保存接口报文；非 HTTP 漏洞不需要报文"), nil
				}
				if !slices.ContainsFunc(interfaces, func(it packetInterfaceDTO) bool { return it.AssetID == assetID }) {
					return actool.Errorf(fmt.Sprintf("asset_id=%d 不是本漏洞锚定的接口；本漏洞的接口 id：%s。请按接口分别保存，不要套用别的接口的报文",
						assetID, packetInterfaceIDs(interfaces))), nil
				}
				v, err := s.m.pg.SaveRetestPacket(ctx, f.ID, assetID, f.AssetIDs, db.RetestPacketInput{
					AssetID:      assetID,
					RawRequest:   a.RawRequest,
					RawResponse:  a.RawResponse,
					GroundedRefs: a.GroundedRefs,
					Assumptions:  a.Assumptions,
					Generator:    db.FindingRetestAgentKey,
					Source:       db.RetestPacketSourceLLM,
				})
				if err != nil {
					return actool.Errorf("报文保存失败：" + err.Error()), nil
				}
				coverage, err := s.packetCoverageSummary(f)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				revision := 0
				if p, err := s.m.pg.GetFindingPacket(f.ID, assetID); err == nil && p != nil {
					revision = p.Revision
				}
				return jsonResult(map[string]any{
					"saved": true, "asset_id": assetID, "revision": revision,
					"valid": v.Valid, "errors": v.Errors, "warnings": v.Warnings,
					"method": v.Method, "path": v.Path, "host": v.Host,
					"coverage": coverage,
				})
			}),
	}
}

// packetInterfaceIDs 把接口槽位拼成「12、13」这样的可读清单，用于工具报错。
func packetInterfaceIDs(items []packetInterfaceDTO) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, strconv.FormatInt(it.AssetID, 10))
	}
	return strings.Join(parts, "、")
}

// migrateRetesterPrompt 把老库里的「渗透复测」提示词一次性换成「静态复核」版。
//
// 为什么需要它：seedFindingRetester 只在 agent 首次创建时写入提示词（flag
// finding_retester_seed_v1），而 seedPrompts 用的 SeedPromptIfEmpty 一旦有版本就
// 不再覆盖——所以已存在的库会一直留着 v1 那段「用原 PoC 做实际请求/命令并记录响应」
// 的提示词，与本平台不发起任何请求的能力直接矛盾，还会诱导模型编造运行时证据。
//
// 与 prompts.codeaudit_v1 同一套路：用 settings 打标只做一次，之后用户编辑照旧保留。
func (s *Server) migrateRetesterPrompt() error {
	a, err := s.m.pg.GetAgentByKey(db.FindingRetestAgentKey)
	if err != nil || a == nil {
		return err
	}
	if v, ok, err := s.m.pg.GetSetting(settingRetesterReviewMigrated); err == nil && ok && v == "1" {
		return nil
	}
	if _, err := s.m.pg.SavePrompt(a.ID, agent.RetesterDefaultPrompt, "迁移为漏洞复核版提示词", "system"); err != nil {
		return err
	}
	// 名字/描述也还是「复测」时代的，一并对齐。只在这两栏仍是种子原值时才改：
	// 用户自己改过的名字/描述不动（避免覆盖用户意图）。
	if _, err := s.m.pg.Exec(`
UPDATE agents SET name='漏洞复核', description='从漏洞详情手动启动，读取原证据与推演报文，回到源码静态复核漏洞是否真实成立并保存独立结论。'
WHERE key=$1 AND name='漏洞复测'`, db.FindingRetestAgentKey); err != nil {
		return err
	}
	return s.m.pg.SetSetting(settingRetesterReviewMigrated, "1")
}

// settingRetesterReviewMigrated marks the one-time retester prompt migration from
// the pentest-era「漏洞复测」body to the「漏洞复核」body.
const settingRetesterReviewMigrated = "retester.review_v1"

// settingRetesterPacketsMigrated marks the one-time retester prompt migration that
// teaches the「复核」body to revise retest packets (save_finding_packet).
const settingRetesterPacketsMigrated = "retester.packets_v1"

// migrateRetesterPromptPackets 给已存在的库补上「复核可修订报文」这段提示词。
//
// 为什么不能只改 RetesterDefaultPrompt：seedFindingRetester 只在 agent 首次创建时
// 写提示词，SeedPromptIfEmpty 又是一旦有版本就不再覆盖——老库会一直停在「复核只能读
// 报文、不能改」的旧文本上，模型即使拿到了新工具也不会用。与 report_finding 的
// retest_packets schema 迁移同一个问题，只是这次改的是提示词。
//
// 只覆盖「从未被用户改过」的提示词（updated_by='system'，即仍是内置默认或上一次系统
// 迁移写的）：用户在 Agent 管理里改过的正文不动，否则每次升级都吞掉用户改动。工具
// 自身的 description 里已经写全了规约，用户自定义的提示词不会因此失去这个能力。
func (s *Server) migrateRetesterPromptPackets() error {
	if v, ok, err := s.m.pg.GetSetting(settingRetesterPacketsMigrated); err == nil && ok && v == "1" {
		return nil
	}
	a, err := s.m.pg.GetAgentByKey(db.FindingRetestAgentKey)
	if err != nil || a == nil {
		return err
	}
	var text string
	var by sql.NullString
	err = s.m.pg.QueryRow(`SELECT p.template_text, p.updated_by FROM agents a
		JOIN agent_prompts p ON p.id=a.current_prompt_id WHERE a.id=$1`, a.ID).Scan(&text, &by)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 还没有任何提示词版本：交给 seedPrompts/SeedPromptIfEmpty，不在这里造一条。
	case err != nil:
		return err
	case by.String == "system" && !strings.Contains(text, saveFindingPacketTool):
		if _, err := s.m.pg.SavePrompt(a.ID, agent.RetesterDefaultPrompt, "补充复核报文修订能力", "system"); err != nil {
			return err
		}
	}
	return s.m.pg.SetSetting(settingRetesterPacketsMigrated, "1")
}

// Seed the editable agent atomically, without an automatic discovery trigger.
// Once seeded, user edits/deletion survive restarts; a pre-existing key is kept.
func (s *Server) seedFindingRetester() error {
	for _, t := range s.findingRetestTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings, _ := json.Marshal([]string{db.FindingRetestAgentKey})
		if err := s.m.pg.SeedTool(t.Name(), t.Description(), schema, bindings); err != nil {
			return err
		}
	}
	const flag = "finding_retester_seed_v1"
	tx, err := s.m.pg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock this migration, including concurrent server initialization.
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(7337741010)`); err != nil {
		return err
	}
	var done string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key=$1`, flag).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done == "true" {
		return nil
	}
	var id int64
	err = tx.QueryRow(`INSERT INTO agents(key,name,description,role,builtin,enabled)
	VALUES ($1,'漏洞复核','从漏洞详情手动启动，读取原证据与推演报文，回到源码静态复核漏洞是否真实成立并保存独立结论。','assistant',false,true)
	ON CONFLICT (key) DO NOTHING RETURNING id`, db.FindingRetestAgentKey).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if id > 0 {
		var pid int64
		if err = tx.QueryRow(`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by)
		VALUES ($1,1,$2,'内置默认','system') RETURNING id`, id, agent.RetesterDefaultPrompt).Scan(&pid); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, pid, id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES ($1,'true') ON CONFLICT(key) DO UPDATE SET value='true'`, flag); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) finishRetest(id int64, status, reason string) {
	if err := s.m.pg.FinishFindingRetest(id, status, reason); err != nil {
		// Surface failure to the conversation runner rather than inventing a result.
		log.Printf("[retest %d] finish: %v", id, err)
	}
}
