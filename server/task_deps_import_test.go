package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/javadeps"
)

// =====================================================================
// 本地 Maven 仓库导入的端到端验收（见 docs/java-m2依赖读取与反编译方案.md §7.1）
//
// 这一组测试盯的是三条红线，任何一条被破坏都应当失败：
//  1. 「配一次、所有任务全局可用」：只配一个仓库路径，任务就能按坐标取件，不用上传；
//  2. 「只取被点名的那一个」：同一个仓库里的邻居坐标绝不能被顺手带进来；
//  3. 「按需」不被破坏：导入只建清单与类名索引，deps/ 下必须是 0 个 .java。
// =====================================================================

// createDepsTask creates a task through the real HTTP API. The deps handlers look the
// task up in the manager's in-memory registry, so going through the API (rather than
// inserting a row) is what production does and the only thing the handlers can see.
func createDepsTask(t *testing.T, s *Server, h http.Handler, tok string) (string, int64) {
	t.Helper()
	cb, _ := json.Marshal(map[string]any{
		"description": "本地仓库导入测试", "goal": "验证按坐标导入", "seedFirstIntent": false,
		// 源码是建任务的必填项（所有账号一致），先造一个 ready 的 staging 上传。
		"upload_id": stageTestSource(t, s, "depsimport"),
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
	return tid, mustI64(t, tid)
}

// writeRepoJar lays a real jar into a fake local Maven repository.
func writeRepoJar(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// postJSON issues an authenticated JSON POST and returns the recorder.
func postJSON(t *testing.T, h http.Handler, tok, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", path, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// getDepsList fetches and decodes the dependency panel's payload.
func getDepsList(t *testing.T, h http.Handler, tok, tid string) DependencyListDTO {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/tasks/"+tid+"/deps", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("GET deps 失败: %d %s", rec.Code, rec.Body.String())
	}
	var out DependencyListDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 deps 响应失败: %v", err)
	}
	return out
}

// countJavaFilesUnder counts .java files below root (0 is the expected steady state).
func countJavaFilesUnder(t *testing.T, root string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".java") {
			n++
		}
		return nil
	})
	return n
}

// importFromRepo runs one import request and decodes the result.
func importFromRepo(t *testing.T, h http.Handler, tok, tid string, body map[string]any) DepsImportResult {
	t.Helper()
	rec := postJSON(t, h, tok, "/api/tasks/"+tid+"/deps/import", body)
	if rec.Code != 200 {
		t.Fatalf("导入失败: %d %s", rec.Code, rec.Body.String())
	}
	var res DepsImportResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("解析导入响应失败: %v", err)
	}
	return res
}

func TestDepsAPIImportFromLocalRepo(t *testing.T) {
	s, pg := depsServer(t)
	h := s.Handler()
	tok := testAdminToken(t, s)

	// A fake repository holding exactly two coordinates.
	repoRoot := t.TempDir()
	writeRepoJar(t, repoRoot, "org/apache/logging/log4j/log4j-core/2.17.0/log4j-core-2.17.0.jar",
		makeJarForTest(t, "org.apache.logging.log4j", "log4j-core", "2.17.0"))
	writeRepoJar(t, repoRoot, "antlr/antlr/2.7.2/antlr-2.7.2.jar",
		makeJarForTest(t, "antlr", "antlr", "2.7.2"))
	t.Setenv("ARTEX_JAVA_LOCAL_REPO", repoRoot)

	tid, taskID := createDepsTask(t, s, h, tok)
	defer pg.DeleteTask(taskID) //nolint:errcheck

	// --- the capability is advertised before anything is imported ---
	before := getDepsList(t, h, tok, tid)
	if !before.LocalRepoAvailable || before.LocalRepo == "" {
		t.Fatalf("本地仓库能力没有上报给前端: available=%v path=%q", before.LocalRepoAvailable, before.LocalRepo)
	}
	if len(before.Deps) != 0 {
		t.Fatalf("新任务不该有依赖，得到 %d 个", len(before.Deps))
	}

	// --- import one coordinate ---
	res := importFromRepo(t, h, tok, tid, map[string]any{
		"gavs": []string{"org.apache.logging.log4j:log4j-core:2.17.0"},
	})
	if res.Imported != 1 || res.Already != 0 || res.NotFound != 0 || res.Skipped != 0 {
		t.Fatalf("导入统计不对: %+v", res)
	}
	if len(res.Results) != 1 || res.Results[0].Status != depImportImported {
		t.Fatalf("单条结果不对: %+v", res.Results)
	}
	if res.Jars != 1 || res.Dependencies != 1 {
		t.Errorf("jars=%d dependencies=%d，期望 1/1", res.Jars, res.Dependencies)
	}
	if res.IndexedClass == 0 {
		t.Error("类名索引应当已建立")
	}
	if res.LocalRepo == "" {
		t.Error("响应应当回带本地仓库路径")
	}

	// The jar really landed in the task, in Maven repository layout.
	jarAbs := filepath.Join(s.m.dir, "tasks", tid, "deps", "jars",
		"org", "apache", "logging", "log4j", "log4j-core", "2.17.0", "log4j-core-2.17.0.jar")
	if _, err := os.Stat(jarAbs); err != nil {
		t.Fatalf("jar 没有落盘: %v", err)
	}

	// --- red line 2: the sibling coordinate in the same repository was NOT taken ---
	if _, err := os.Stat(filepath.Join(s.m.dir, "tasks", tid, "deps", "jars", "antlr")); !os.IsNotExist(err) {
		t.Error("只应导入被点名的坐标，antlr 不该被带进来")
	}

	// --- red line 3: importing still decompiles nothing ---
	if n := countJavaFilesUnder(t, javadeps.TaskDepsDir(s.m.dir, tid)); n != 0 {
		t.Errorf("导入后 deps/ 下有 %d 个 .java —— 「按需反编译」被破坏", n)
	}

	// The panel now shows the dependency with a trusted coordinate.
	after := getDepsList(t, h, tok, tid)
	if len(after.Deps) != 1 {
		t.Fatalf("依赖数 = %d，期望 1", len(after.Deps))
	}
	if got := after.Deps[0].GAV; got != "org.apache.logging.log4j:log4j-core:2.17.0" {
		t.Errorf("坐标 = %q", got)
	}
	if after.TotalClasses == 0 {
		t.Error("类名索引条数应当大于 0")
	}

	// --- importing the same coordinate again is a no-op, not a duplicate ---
	again := importFromRepo(t, h, tok, tid, map[string]any{
		"gavs": []string{"org.apache.logging.log4j:log4j-core:2.17.0"},
	})
	if again.Imported != 0 || again.Already != 1 {
		t.Errorf("重复导入应当记为 already: %+v", again)
	}

	// --- a coordinate the repository does not hold ---
	missing := importFromRepo(t, h, tok, tid, map[string]any{
		"gavs": []string{"com.example:absent:1.0"},
	})
	if missing.NotFound != 1 || len(missing.Results) != 1 || missing.Results[0].Status != depImportNotFound {
		t.Fatalf("缺失坐标的结果不对: %+v", missing)
	}
	if !strings.Contains(missing.Results[0].Error, "com/example/absent/1.0") {
		t.Errorf("错误信息应当指出查找路径，得到 %q", missing.Results[0].Error)
	}

	// --- a coordinate that would escape the repository is refused up front ---
	escape := importFromRepo(t, h, tok, tid, map[string]any{
		"gavs": []string{"../../etc:passwd:1"},
	})
	if escape.Skipped != 1 || len(escape.Results) != 1 || escape.Results[0].Status != depImportInvalid {
		t.Fatalf("越界坐标应当被拒绝: %+v", escape)
	}

	// --- a coordinate without a groupId cannot be resolved from a repository ---
	noGroup := importFromRepo(t, h, tok, tid, map[string]any{
		"gavs": []string{"antlr:2.7.2"},
	})
	if noGroup.NotFound != 1 || !strings.Contains(noGroup.Results[0].Error, "groupId") {
		t.Errorf("缺 groupId 应当给出可读的原因: %+v", noGroup.Results)
	}

	// --- scan_source with no uploaded source says so instead of failing ---
	noSource := importFromRepo(t, h, tok, tid, map[string]any{"scan_source": true})
	if noSource.ScannedPoms != 0 {
		t.Errorf("没有源码包时不应扫到 pom，得到 %d", noSource.ScannedPoms)
	}
	if len(noSource.Notes) == 0 {
		t.Error("应当有一条说明「没有源码包」的 note")
	}
}

// pomForImportTest is a source project's pom: one resolvable direct dependency, one
// whose version only the parent/BOM could supply, and one test-scope dependency.
const pomForImportTest = `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.example</groupId>
  <artifactId>app</artifactId>
  <version>1.0</version>
  <dependencies>
    <dependency>
      <groupId>antlr</groupId>
      <artifactId>antlr</artifactId>
      <version>2.7.2</version>
    </dependency>
    <dependency>
      <groupId>com.example</groupId>
      <artifactId>ghost</artifactId>
      <version>${ghost.version}</version>
    </dependency>
    <dependency>
      <groupId>com.example</groupId>
      <artifactId>testonly</artifactId>
      <version>1.0</version>
      <scope>test</scope>
    </dependency>
  </dependencies>
</project>`

// TestDepsAPIImportScansSourcePoms covers the "我不知道上传哪些" half of the feature:
// the coordinates come from the uploaded project's own pom.xml, so the user never has
// to run Maven on the server to produce a list.
func TestDepsAPIImportScansSourcePoms(t *testing.T) {
	s, pg := depsServer(t)
	h := s.Handler()
	tok := testAdminToken(t, s)

	repoRoot := t.TempDir()
	writeRepoJar(t, repoRoot, "antlr/antlr/2.7.2/antlr-2.7.2.jar",
		makeJarForTest(t, "antlr", "antlr", "2.7.2"))
	t.Setenv("ARTEX_JAVA_LOCAL_REPO", repoRoot)

	tid, taskID := createDepsTask(t, s, h, tok)
	defer pg.DeleteTask(taskID) //nolint:errcheck

	// Stand up the source tree exactly the way an upload does: files under
	// data/tasks/<id>/source plus tasks.source_path pointing at them.
	srcDir := filepath.Join(s.m.dir, "tasks", tid, "source")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "pom.xml"), []byte(pomForImportTest), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := filepath.ToSlash(filepath.Join("tasks", tid, "source"))
	if err := pg.SetTaskSourcePath(taskID, rel); err != nil {
		t.Fatalf("写入 source_path 失败: %v", err)
	}
	s.m.SetTaskSourcePathInMemory(tid, rel)

	res := importFromRepo(t, h, tok, tid, map[string]any{"scan_source": true})
	if res.ScannedPoms != 1 {
		t.Fatalf("scanned_poms = %d，期望 1", res.ScannedPoms)
	}
	if res.Imported != 1 {
		t.Errorf("应当导入 pom 里那一个可解析的依赖: %+v", res.Results)
	}

	// The unresolvable ${ghost.version} is reported with its raw text, never guessed.
	var unresolved *DepsImportEntry
	for i := range res.Results {
		if res.Results[i].Status == depImportUnresolved {
			unresolved = &res.Results[i]
		}
	}
	if unresolved == nil {
		t.Fatalf("${ghost.version} 应当被报为 unresolved_version: %+v", res.Results)
	}
	if !strings.Contains(unresolved.GAV, "${ghost.version}") {
		t.Errorf("未解析条目的 GAV 应当保留原文，得到 %q", unresolved.GAV)
	}

	// test-scope dependencies do not ship, so they are skipped — and said so.
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "test scope") {
		t.Errorf("应当说明跳过了 test scope 依赖，notes=%v", res.Notes)
	}

	// The imported dependency is recorded as a direct, pom-declared dependency.
	deps := getDepsList(t, h, tok, tid)
	if len(deps.Deps) != 1 {
		t.Fatalf("依赖数 = %d，期望 1", len(deps.Deps))
	}
	if !deps.Deps[0].Direct {
		t.Error("pom.xml 声明的依赖应当标记为 direct")
	}

	// A rescan must not wipe the scope/direct information the pom scan produced.
	rr := postJSON(t, h, tok, "/api/tasks/"+tid+"/deps/rescan", map[string]any{})
	if rr.Code != 200 {
		t.Fatalf("重新扫描失败: %d %s", rr.Code, rr.Body.String())
	}
	depsAfter := getDepsList(t, h, tok, tid)
	if len(depsAfter.Deps) != 1 || !depsAfter.Deps[0].Direct {
		t.Errorf("重新扫描后 direct 标记丢失: %+v", depsAfter.Deps)
	}
}

// TestDepsAPIImportRequiresLocalRepo pins the failure mode when the feature is off:
// a clear, actionable error rather than a silent no-op.
func TestDepsAPIImportRequiresLocalRepo(t *testing.T) {
	s, pg := depsServer(t)
	h := s.Handler()
	tok := testAdminToken(t, s)

	// Point at a path that does not exist: a typo must degrade to "disabled", not crash.
	t.Setenv("ARTEX_JAVA_LOCAL_REPO", filepath.Join(t.TempDir(), "not-a-repo"))

	tid, taskID := createDepsTask(t, s, h, tok)
	defer pg.DeleteTask(taskID) //nolint:errcheck

	before := getDepsList(t, h, tok, tid)
	if before.LocalRepoAvailable {
		t.Error("不存在的仓库路径应当上报为不可用")
	}
	if before.LocalRepo != "" {
		t.Errorf("不可用的仓库不应回带路径，得到 %q", before.LocalRepo)
	}

	rec := postJSON(t, h, tok, "/api/tasks/"+tid+"/deps/import", map[string]any{
		"gavs": []string{"antlr:antlr:2.7.2"},
	})
	if rec.Code != 503 {
		t.Fatalf("未配置本地仓库应当返回 503，得到 %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "local_repo") {
		t.Errorf("错误信息应当告诉用户配哪个字段，得到 %s", rec.Body.String())
	}
}
