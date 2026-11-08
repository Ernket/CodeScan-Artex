package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// 没有可用 LLM 时，「开启任务」必须明确拒绝，而不是返回 200 让任务永远停在 created。
// 背景：任务状态由 resolvedTaskStatus 派生——只有 engine.ReadyFor(t) 为真且已准入才显示
// running；没有 provider 时用户点开启会看到「成功提示 + 状态不变」，无法判断该去配 LLM。
// 这里把默认 profile 摘掉后再启动 server，模拟「LLM 未配置」的环境。
func TestStartTaskWithoutLLMIsRejected(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	// 记下原默认 profile，结束后恢复，避免污染共享测试库。
	var prevDefault int64
	if err := pg.QueryRow(`SELECT COALESCE(MIN(id),0) FROM llm_profiles WHERE is_default`).Scan(&prevDefault); err != nil {
		pg.Close()
		t.Skipf("llm_profiles unavailable (%v) — skipping", err)
	}
	if _, err := pg.Exec(`UPDATE llm_profiles SET is_default=false`); err != nil {
		pg.Close()
		t.Fatalf("clear default profile: %v", err)
	}
	pg.Close()
	t.Cleanup(func() {
		restore, err := db.Open(dsn)
		if err != nil {
			return
		}
		defer restore.Close()
		if prevDefault > 0 {
			_, _ = restore.Exec(`UPDATE llm_profiles SET is_default=true WHERE id=$1`, prevDefault)
		}
	})

	// 在「没有默认 profile」的状态下启动 server：engine 不会构建 provider。
	s, _, _ := trafficEvidenceServer(t)
	if s.engine.Ready() {
		t.Skip("engine 已持有 provider（环境里还有其它可用配置），跳过该场景")
	}

	// 造一个 created 任务并拿到它的 id（CreateTask 返回 string id）。
	raw, _ := json.Marshal(map[string]string{"goal": "无 LLM 时开启任务", "upload_id": stageTestSource(t, s, "startllm")})
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+testAdminToken(t, s))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create task: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("bad create payload: %s (%v)", w.Body.String(), err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/tasks/"+created.ID+"/start", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken(t, s))
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 409 {
		t.Fatalf("无 LLM 时开启任务应返回 409，实际 %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "LLM") {
		t.Fatalf("错误信息应提示去配置 LLM：%s", w.Body.String())
	}

	// 任务必须仍可被后续正常开启：状态没有被写坏。
	after, ok := s.m.Task(created.ID)
	if !ok || after == nil {
		t.Fatal("task disappeared")
	}
	if got := s.resolvedTaskStatus(after); got != "created" {
		t.Fatalf("被拒绝的开启不应改变任务状态，实际 %s", got)
	}
}
