package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/javadeps"
	actool "github.com/Autumn-27/norma/tool"
)

// makeJarForTest builds a minimal but REAL jar (a zip with a class entry and a
// pom.properties) so the scan path runs for real instead of against a stub.
func makeJarForTest(t *testing.T, group, artifact, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// A class entry is all ScanJar needs to count classes: it reads the central
	// directory, never the bytecode (that is the whole point of L1).
	entries := map[string]string{
		strings.ReplaceAll(group, ".", "/") + "/demo/Widget.class": "\xca\xfe\xba\xbe",
		strings.ReplaceAll(group, ".", "/") + "/demo/Helper.class": "\xca\xfe\xba\xbe",
		"META-INF/maven/" + group + "/" + artifact + "/pom.properties": "groupId=" + group +
			"\nartifactId=" + artifact + "\nversion=" + version + "\n",
	}
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// depsServer boots a server on a temp data dir against the live PG.
func depsServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	// These tests point a full server at the LIVE database. If that database happens to
	// hold tasks that are still running, the manager boots workers for them and those
	// workers keep writing into the data dir. So the temp dirs are removed best-effort
	// instead of through t.TempDir(), whose RemoveAll asserts success and would fail the
	// test with "TempDir RemoveAll cleanup: The directory is not empty" for a reason that
	// has nothing to do with the dependency code under test.
	dir, err := os.MkdirTemp("", "artex-deps-mgr")
	if err != nil {
		t.Fatal(err)
	}
	td, err := os.MkdirTemp("", "artex-deps-data")
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(dir, "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() {
		_ = m.Close() // stop the workers before pulling the data dir out from under them
		_ = os.RemoveAll(td)
		_ = os.RemoveAll(dir)
	})
	s := New(context.Background(), m, td, td, td)
	return s, m.PG()
}

// TestDepsAPIIngestDoesNotDecompile is the central guarantee of
// docs/java-m2依赖读取与反编译方案.md: uploading a dependency pack builds the manifest
// and the class index but produces ZERO .java. On-demand is the default.
func TestDepsAPIIngestDoesNotDecompile(t *testing.T) {
	s, pg := depsServer(t)
	h := s.Handler()
	tok := testAdminToken(t, s)

	// Create the task through the real HTTP API: the manager keeps an in-memory
	// task registry that pg.CreateTask does not populate, so going through the API
	// is both faithful to production and the only way the handlers can find it.
	cb, _ := json.Marshal(map[string]any{
		"description": "依赖API测试", "goal": "验证入库不反编译", "seedFirstIntent": false,
		// 源码是建任务的必填项（所有账号一致），先造一个 ready 的 staging 上传。
		"upload_id": stageTestSource(t, s, "deps"),
	})
	cr := httptest.NewRequest("POST", "/api/tasks", bytes.NewReader(cb))
	cr.Header.Set("Authorization", "Bearer "+tok)
	cr.Header.Set("Content-Type", "application/json")
	crec := httptest.NewRecorder()
	h.ServeHTTP(crec, cr)
	if crec.Code != 200 && crec.Code != 201 {
		t.Fatalf("创建任务失败: %d %s", crec.Code, crec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(crec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	tid, _ := created["id"].(string)
	if tid == "" {
		t.Fatalf("创建任务未返回 id: %s", crec.Body.String())
	}
	taskID, err := strconv.ParseInt(tid, 10, 64)
	if err != nil {
		t.Fatalf("任务 id 不是数字: %q", tid)
	}
	defer pg.DeleteTask(taskID) //nolint:errcheck

	// Build the upload: a .m2-shaped path plus a flat jar (both layouts supported).
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "deps.zip")
	if err != nil {
		t.Fatal(err)
	}
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	add := func(name string, data []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	add("org/apache/logging/log4j/log4j-core/2.17.0/log4j-core-2.17.0.jar",
		makeJarForTest(t, "org.apache.logging.log4j", "log4j-core", "2.17.0"))
	add("flat/antlr-2.7.2.jar", makeJarForTest(t, "antlr", "antlr", "2.7.2"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zipBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("POST", "/api/tasks/"+tid+"/deps", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("上传依赖失败: %d %s", rec.Code, rec.Body.String())
	}
	var up map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if got := up["jars"]; got != float64(2) {
		t.Errorf("jars = %v, 期望 2", got)
	}

	// --- THE guarantee: nothing was decompiled ---
	depsDir := javadeps.TaskDepsDir(s.m.dir, tid)
	var javaCount int
	_ = filepath.WalkDir(depsDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".java") {
			javaCount++
		}
		return nil
	})
	if javaCount != 0 {
		t.Fatalf("入库后 deps/ 下有 %d 个 .java —— 「默认不反编译」被破坏", javaCount)
	}
	// The index artifacts must exist (that is what the cheap path produces).
	for _, want := range []string{"INDEX.md", filepath.Join("index", "classes.txt")} {
		if _, err := os.Stat(filepath.Join(depsDir, want)); err != nil {
			t.Errorf("缺少 %s: %v", want, err)
		}
	}
	// INDEX.md must carry the red lines the agent relies on.
	md, err := os.ReadFile(filepath.Join(depsDir, "INDEX.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"反编译产物，不是原始源码", "不可信输入", "默认不会反编译任何 jar"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("INDEX.md 缺少红线提示 %q", want)
		}
	}
	// Class index must be greppable and keyed by coordinate.
	idx, err := os.ReadFile(filepath.Join(depsDir, "index", "classes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(idx), "org.apache.logging.log4j:log4j-core:2.17.0") {
		t.Error("类名索引里没有按坐标索引的条目")
	}

	// --- coordinates must be authoritative when pom.properties exists ---
	rows, err := pg.Dependencies().ListDependencies(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("依赖数 = %d, 期望 2", len(rows))
	}
	byGAV := map[string]*db.Dependency{}
	for _, d := range rows {
		byGAV[d.GAV] = d
		if d.SourceState != string(javadeps.SourceNone) {
			t.Errorf("%s 的 source_state = %q, 期望 none", d.GAV, d.SourceState)
		}
	}
	l4j := byGAV["org.apache.logging.log4j:log4j-core:2.17.0"]
	if l4j == nil {
		t.Fatalf("没有解析出 log4j 坐标, 实际: %v", keysOf(byGAV))
	}
	if l4j.GAVSource != string(javadeps.GAVSrcPom) {
		t.Errorf("log4j 坐标来源 = %q, 期望 pom.properties", l4j.GAVSource)
	}
	if l4j.ClassCount != 2 {
		t.Errorf("log4j 类数 = %d, 期望 2", l4j.ClassCount)
	}
	// The flat jar has no repository path; its coordinate comes from the file name
	// and must be labelled as such (it is a guess, not authoritative).
	antlr := byGAV["antlr:antlr:2.7.2"]
	if antlr == nil {
		t.Fatalf("没有解析出 antlr 坐标, 实际: %v", keysOf(byGAV))
	}
	if antlr.GAVSource != string(javadeps.GAVSrcPom) && antlr.GAVSource != string(javadeps.GAVSrcFilename) {
		t.Errorf("antlr 坐标来源异常: %q", antlr.GAVSource)
	}

	// --- GET returns the panel payload with capability info ---
	r2 := httptest.NewRequest("GET", "/api/tasks/"+tid+"/deps", nil)
	r2.Header.Set("Authorization", "Bearer "+tok)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, r2)
	if rec2.Code != 200 {
		t.Fatalf("GET deps 失败: %d %s", rec2.Code, rec2.Body.String())
	}
	var list DependencyListDTO
	if err := json.Unmarshal(rec2.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Deps) != 2 || list.Materialized != 0 {
		t.Errorf("列表 = %d 个依赖 / 已反编译 %d, 期望 2 / 0", len(list.Deps), list.Materialized)
	}
	if list.TotalClasses != 4 {
		t.Errorf("类索引总数 = %d, 期望 4", list.TotalClasses)
	}
	if list.Engine == "" {
		t.Error("engine 不应为空（UI 要显示当前能力）")
	}
	for _, d := range list.Deps {
		if !d.CanMaterialize {
			t.Errorf("%s 应该可以按需反编译", d.GAV)
		}
	}

	// --- clear removes rows and the tree ---
	r3 := httptest.NewRequest("POST", "/api/tasks/"+tid+"/deps/clear", nil)
	r3.Header.Set("Authorization", "Bearer "+tok)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, r3)
	if rec3.Code != 200 {
		t.Fatalf("清空依赖失败: %d %s", rec3.Code, rec3.Body.String())
	}
	if n, _ := pg.Dependencies().CountDependencies(taskID); n != 0 {
		t.Errorf("清空后仍有 %d 行依赖", n)
	}
	if _, err := os.Stat(depsDir); !os.IsNotExist(err) {
		t.Error("清空后 deps 目录仍存在")
	}
}

// TestDependencyToolSeedsAreBounded locks the security property that makes these
// host tools safe: they are seeded with an explicit agent list, so ToolResolve
// keeps them out of every other agent's tool set. (An unseeded host tool would be
// passed through to ALL agents by ToolResolve.)
func TestDependencyToolSeedsAreBounded(t *testing.T) {
	s, _ := depsServer(t)
	seeds := s.dependencyToolSeeds()
	if len(seeds) != 2 {
		t.Fatalf("依赖工具 seed 数 = %d, 期望 2", len(seeds))
	}
	seen := map[string]bool{}
	for _, sd := range seeds {
		seen[sd.Key] = true
		if sd.Desc == "" || len(sd.Schema) == 0 {
			t.Errorf("%s 的 seed 缺少描述或 schema", sd.Key)
		}
		want := []string{"planner", "worker"}
		if len(sd.Agents) != len(want) {
			t.Fatalf("%s 绑定 = %v, 期望 %v", sd.Key, sd.Agents, want)
		}
		for i := range want {
			if sd.Agents[i] != want[i] {
				t.Errorf("%s 绑定 = %v, 期望 %v", sd.Key, sd.Agents, want)
			}
		}
	}
	for _, k := range []string{"list_dependencies", "ensure_dependency_source"} {
		if !seen[k] {
			t.Errorf("缺少工具 seed: %s", k)
		}
	}

	// Tool shape: the listing tool is read-only; the materialising one is NOT
	// (it writes a source tree and starts a JVM), and must not run concurrently.
	tools := s.dependencyTools(42)
	byName := map[string]actool.CoreTool{}
	for _, tl := range tools {
		byName[tl.Name()] = tl
	}
	ls := byName["list_dependencies"]
	if ls == nil || !ls.IsReadOnly(nil) {
		t.Error("list_dependencies 应当是只读工具")
	}
	ens := byName["ensure_dependency_source"]
	if ens == nil {
		t.Fatal("缺少 ensure_dependency_source")
	}
	if ens.IsReadOnly(nil) {
		t.Error("ensure_dependency_source 会写文件并启动 JVM，不应标成只读")
	}
	if ens.IsConcurrencySafe(nil) {
		t.Error("反编译很重，不应并发执行")
	}
	// It must require the coordinate.
	schema, _ := ens.InputSchema()["required"].([]any)
	if len(schema) != 1 || schema[0] != "gav" {
		t.Errorf("ensure_dependency_source 必填参数 = %v, 期望 [gav]", schema)
	}
}

// TestDependencyToolGuards verifies the agent-facing guard rails without needing a
// decompiler: unknown coordinates are refused, and the listing tool explains the
// on-demand model instead of silently returning nothing.
func TestDependencyToolGuards(t *testing.T) {
	s, pg := depsServer(t)
	tk, err := pg.CreateTask("依赖工具测试", "验证工具护栏", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTask(tk.ID) //nolint:errcheck
	taskID := tk.ID

	ctx := context.Background()

	// --- list on a task with no dependencies: must teach, not just be empty ---
	res, err := s.toolListDependencies(taskID).Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(res)
	for _, want := range []string{"没有上传任何第三方依赖", "不要试图自己下载依赖"} {
		if !strings.Contains(text, want) {
			t.Errorf("空依赖清单缺少引导文案 %q，实际:\n%s", want, text)
		}
	}

	// --- a non-task run (chat) must refuse rather than guess a task ---
	res, err = s.toolListDependencies(0).Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(res), "没有关联任务") {
		t.Errorf("taskID=0 时应拒绝，实际: %s", resultText(res))
	}

	// --- unknown coordinate: refuse, and tell the model not to invent one ---
	res, err = s.toolEnsureDependencySource(taskID).Call(ctx, json.RawMessage(`{"gav":"com.nope:missing:1.0"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(res); !strings.Contains(got, "没有坐标") || !strings.Contains(got, "不要臆造") {
		t.Errorf("未知坐标应拒绝并提示不要臆造，实际: %s", got)
	}

	// --- malformed coordinate: explicit error, not a panic ---
	res, err = s.toolEnsureDependencySource(taskID).Call(ctx, json.RawMessage(`{"gav":"just-one-part"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(res), "坐标格式不对") {
		t.Errorf("非法坐标应报格式错误，实际: %s", resultText(res))
	}

	// --- empty gav is rejected ---
	res, err = s.toolEnsureDependencySource(taskID).Call(ctx, json.RawMessage(`{"gav":"  "}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(res), "gav 为必填") {
		t.Errorf("空 gav 应报必填，实际: %s", resultText(res))
	}

	// --- coordinate resolution: canonical, flat, classifier, and lenient forms ---
	if _, err := pg.Dependencies().UpsertDependency(&db.Dependency{
		TaskID: taskID, GroupID: "org.apache.logging.log4j", Artifact: "log4j-core",
		Version: "2.17.0", JarPath: "deps/jars/x/log4j-core-2.17.0.jar", GAVSource: "pom.properties",
	}); err != nil {
		t.Fatal(err)
	}
	for _, gav := range []string{
		"org.apache.logging.log4j:log4j-core:2.17.0",
		"`org.apache.logging.log4j:log4j-core:2.17.0`", // models wrap coordinates in backticks
		" log4j-core:2.17.0 ",                          // group omitted → lenient match
	} {
		got, err := s.findDependencyByGAV(taskID, gav)
		if err != nil {
			t.Errorf("findDependencyByGAV(%q) 报错: %v", gav, err)
			continue
		}
		if got == nil {
			t.Errorf("findDependencyByGAV(%q) 未命中", gav)
			continue
		}
		if got.Artifact != "log4j-core" {
			t.Errorf("findDependencyByGAV(%q) 命中错误依赖: %s", gav, got.Artifact)
		}
	}

	// --- list now reports the dependency and the default state ---
	res, err = s.toolListDependencies(taskID).Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	text = resultText(res)
	for _, want := range []string{"org.apache.logging.log4j:log4j-core:2.17.0", "未反编译(按需)", "现在【没有任何依赖被反编译】"} {
		if !strings.Contains(text, want) {
			t.Errorf("依赖清单缺少 %q，实际:\n%s", want, text)
		}
	}
}

// TestDependencyToolRunawayGuard verifies the cap that keeps "on demand" from
// degenerating into "decompile everything" during an autonomous run.
func TestDependencyToolRunawayGuard(t *testing.T) {
	if maxMaterializedPerTask != 20000 {
		t.Fatalf("默认单任务上限 = %d, 期望 20000", maxMaterializedPerTask)
	}
	// The guard compares against the real default (20000). Shrink it for this test so
	// we exercise the very same branch without inserting tens of thousands of rows.
	orig := maxMaterializedPerTask
	maxMaterializedPerTask = 40
	t.Cleanup(func() { maxMaterializedPerTask = orig })

	s, pg := depsServer(t)
	tk, err := pg.CreateTask("依赖上限测试", "验证单任务反编译上限", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTask(tk.ID) //nolint:errcheck
	taskID := tk.ID

	// Pretend maxMaterializedPerTask dependencies are already readable.
	for i := 0; i < maxMaterializedPerTask; i++ {
		if _, err := pg.Dependencies().UpsertDependency(&db.Dependency{
			TaskID:    taskID,
			Artifact:  "lib" + strconv.Itoa(i),
			Version:   "1.0",
			JarPath:   "deps/jars/lib" + strconv.Itoa(i) + "-1.0.jar",
			GAVSource: "filename",
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := pg.Dependencies().ListDependencies(taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rows {
		if err := pg.Dependencies().MarkMaterialized(d.ID, "decompiled", "deps/src/x", 1, 1, "cfr", ""); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.materializedCount(taskID); err != nil || n != maxMaterializedPerTask {
		t.Fatalf("materializedCount = %d (err=%v), 期望 %d", n, err, maxMaterializedPerTask)
	}

	res, err := s.toolEnsureDependencySource(taskID).Call(
		context.Background(), json.RawMessage(`{"gav":"lib0:1.0"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := resultText(res)
	// lib0 is already readable, so the guard isn't reached — it should just report
	// the existing path. Use an un-materialised target to hit the cap instead.
	if !strings.Contains(got, "源码已就绪") {
		t.Errorf("已反编译的依赖应直接返回路径，实际: %s", got)
	}

	if _, err := pg.Dependencies().UpsertDependency(&db.Dependency{
		TaskID: taskID, Artifact: "extra", Version: "9.9",
		JarPath: "deps/jars/extra-9.9.jar", GAVSource: "filename",
	}); err != nil {
		t.Fatal(err)
	}
	res, err = s.toolEnsureDependencySource(taskID).Call(
		context.Background(), json.RawMessage(`{"gav":"extra:9.9"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	got = resultText(res)
	if !strings.Contains(got, "达到单任务上限") {
		t.Errorf("超过上限应被拒绝，实际: %s", got)
	}
	if !strings.Contains(got, "让用户在任务详情页手动反编译") {
		t.Errorf("上限提示应给出人工出口，实际: %s", got)
	}
}

// TestDepsRoutesDoNotConflict guards the Go 1.22 ServeMux ambiguity that would
// panic at registration time (DELETE /api/tasks/{id}/deps vs the pre-existing
// DELETE /api/tasks/uploads/{id}).
func TestDepsRoutesDoNotConflict(t *testing.T) {
	s, _ := depsServer(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("路由注册 panic（模式二义）: %v", r)
		}
	}()
	h := s.Handler()
	// The dependency endpoints must actually be routed (401 without a token, not 404).
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/tasks/1/deps"},
		{"POST", "/api/tasks/1/deps"},
		{"POST", "/api/tasks/1/deps/rescan"},
		{"POST", "/api/tasks/1/deps/clear"},
		{"POST", "/api/tasks/1/deps/7/materialize"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s %s 未注册（404）", tc.method, tc.path)
		}
	}
}

// --- helpers ---

func mustI64(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("任务 id %q 不是数字: %v", s, err)
	}
	return v
}

func keysOf(m map[string]*db.Dependency) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// resultText flattens a tool result into its text, so assertions read naturally.
func resultText(r actool.Result) string {
	var sb strings.Builder
	for _, c := range r.Content {
		if c.Text != "" {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 {
		b, _ := json.Marshal(r)
		return string(b)
	}
	return sb.String()
}

var _ = agent.RunInfo{}
