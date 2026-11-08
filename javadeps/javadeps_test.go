package javadeps

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- coordinate parsing (pure) ---

func TestParseGAV(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"org.apache.logging.log4j:log4j-core:2.17.0", "org.apache.logging.log4j:log4j-core:2.17.0", false},
		{"org.apache.logging.log4j:log4j-core:2.17.0:sources", "org.apache.logging.log4j:log4j-core:2.17.0:sources", false},
		{"antlr:antlr:2.7.2", "antlr:antlr:2.7.2", false},
		{"flat-artifact:1.0", "flat-artifact:1.0", false},
		// 2-part form is artifact:version (flat repository); "a:b" is therefore valid.
		{"a:b", "a:b", false},
		{"", "", true},
		{"a:b:c:d:e", "", true},
	}
	for _, c := range cases {
		got, err := ParseGAV(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseGAV(%q) 期望报错，实际成功", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseGAV(%q) 报错: %v", c.in, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("ParseGAV(%q) = %q, 期望 %q", c.in, got.String(), c.want)
		}
	}
}

func TestGAVRepoPathAndJarName(t *testing.T) {
	g, err := ParseGAV("org.apache.logging.log4j:log4j-core:2.17.0")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := g.RepoPath(), "org/apache/logging/log4j/log4j-core/2.17.0"; got != want {
		t.Errorf("RepoPath() = %q, 期望 %q", got, want)
	}
	if got, want := g.JarName(), "log4j-core-2.17.0.jar"; got != want {
		t.Errorf("JarName() = %q, 期望 %q", got, want)
	}
	if got, want := g.ClassifierJarName("sources"), "log4j-core-2.17.0-sources.jar"; got != want {
		t.Errorf("ClassifierJarName(sources) = %q, 期望 %q", got, want)
	}
	if got, want := SourceRelPath(g), filepath.Join("deps", "src", "org", "apache", "logging", "log4j", "log4j-core", "2.17.0"); got != want {
		t.Errorf("SourceRelPath() = %q, 期望 %q", got, want)
	}
}

func TestGAVFromRepoPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"org/apache/logging/log4j/log4j-core/2.17.0/log4j-core-2.17.0.jar",
			"org.apache.logging.log4j:log4j-core:2.17.0", true},
		{"org/apache/logging/log4j/log4j-core/2.17.0/log4j-core-2.17.0-sources.jar",
			"org.apache.logging.log4j:log4j-core:2.17.0:sources", true},
		{"antlr/antlr/2.7.2/antlr-2.7.2.jar", "antlr:antlr:2.7.2", true},
		// File name does not match artifact-version → not a repository layout.
		{"a/b/1.0/other-9.9.jar", "", false},
		// Too shallow.
		{"log4j-core-2.17.0.jar", "", false},
	}
	for _, c := range cases {
		got, ok := GAVFromRepoPath(c.in)
		if ok != c.ok {
			t.Errorf("GAVFromRepoPath(%q) ok=%v, 期望 %v", c.in, ok, c.ok)
			continue
		}
		if ok && got.String() != c.want {
			t.Errorf("GAVFromRepoPath(%q) = %q, 期望 %q", c.in, got.String(), c.want)
		}
	}
}

func TestParseJarFilename(t *testing.T) {
	cases := []struct {
		in       string
		artifact string
		version  string
		classif  string
	}{
		{"log4j-core-2.17.0.jar", "log4j-core", "2.17.0", ""},
		{"antlr-2.7.2.jar", "antlr", "2.7.2", ""},
		{"commons-lang3-3.12.0.jar", "commons-lang3", "3.12.0", ""},
		{"log4j-core-2.17.0-sources.jar", "log4j-core", "2.17.0", "sources"},
		{"log4j-1.2-api-2.17.0.jar", "log4j-1.2-api", "2.17.0", ""},
	}
	for _, c := range cases {
		g, ok := ParseJarFilename(c.in)
		if !ok {
			t.Errorf("ParseJarFilename(%q) 失败", c.in)
			continue
		}
		if g.Artifact != c.artifact || g.Version != c.version || g.Classifier != c.classif {
			t.Errorf("ParseJarFilename(%q) = %s/%s/%s, 期望 %s/%s/%s",
				c.in, g.Artifact, g.Version, g.Classifier, c.artifact, c.version, c.classif)
		}
	}
}

// --- real-jar tests -----------------------------------------------------------
//
// These run against a real artifact from the developer's local Maven repository
// when one is present, and skip otherwise, so the suite still passes on a machine
// with no ~/.m2.

// findLocalJar locates a jar in the local Maven repository. Returns "" when the
// repository or the artifact is absent.
func findLocalJar(rel string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".m2", "repository", filepath.FromSlash(rel))
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	return ""
}

const log4jRel = "org/apache/logging/log4j/log4j-core/2.17.0/log4j-core-2.17.0.jar"

func TestScanJarRealLog4j(t *testing.T) {
	jar := findLocalJar(log4jRel)
	if jar == "" {
		t.Skip("本地 ~/.m2 没有 log4j-core-2.17.0，跳过真实 jar 扫描")
	}
	info, err := ScanJar(jar, "org/apache/logging/log4j/log4j-core/2.17.0/log4j-core-2.17.0.jar")
	if err != nil {
		t.Fatalf("ScanJar 失败: %v", err)
	}
	if info.Corrupt {
		t.Fatalf("jar 被判定为损坏: %s", info.Err)
	}
	// The coordinate must come from pom.properties, i.e. be authoritative.
	if info.GAVSource != GAVSrcPom {
		t.Errorf("GAV 来源 = %q, 期望 %q（应从 jar 内 pom.properties 读取）", info.GAVSource, GAVSrcPom)
	}
	if got, want := info.GAV.String(), "org.apache.logging.log4j:log4j-core:2.17.0"; got != want {
		t.Errorf("GAV = %q, 期望 %q", got, want)
	}
	if info.ClassCount < 500 {
		t.Errorf("类数 = %d, 期望 >= 500（log4j-core 是大 jar）", info.ClassCount)
	}
	if info.SHA256 == "" || len(info.SHA256) != 64 {
		t.Errorf("sha256 异常: %q", info.SHA256)
	}
	if info.IsSources {
		t.Error("主 jar 不应被判定为 sources jar")
	}
	// The class index must contain the class the whole feature exists to make readable.
	found := false
	for _, c := range info.Classes {
		if c == "org.apache.logging.log4j.core.lookup.JndiLookup" {
			found = true
			break
		}
	}
	if !found {
		t.Error("类索引里没有 org.apache.logging.log4j.core.lookup.JndiLookup")
	}
	if pkgs := info.TopPackages(3); len(pkgs) == 0 {
		t.Error("TopPackages 为空")
	}
}

// testDecompiler builds a Decompiler from the environment. Set ARTEX_CFR_JAR (and
// optionally ARTEX_VINEFLOWER_JAR) to enable the decompile tests.
func testDecompiler(t *testing.T) *Decompiler {
	t.Helper()
	d := &Decompiler{
		JavaPath:      FindJava(""),
		CFRJar:        strings.TrimSpace(os.Getenv("ARTEX_CFR_JAR")),
		VineflowerJar: strings.TrimSpace(os.Getenv("ARTEX_VINEFLOWER_JAR")),
		Timeout:       5 * time.Minute,
		Logf:          t.Logf,
	}
	if d.JavaPath == "" || d.PickEngine() == EngineNone {
		t.Skipf("没有可用的反编译器（java=%q engine=%s），跳过反编译测试", d.JavaPath, d.PickEngine())
	}
	return d
}

// TestDecompileRealJar is the golden acceptance test from the design doc: decompile
// log4j-core and require the JNDI lookup implementation to be readable at the
// expected path, with its JNDI call visible.
func TestDecompileRealJar(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过（反编译一个 jar 需要几十秒）")
	}
	jar := findLocalJar(log4jRel)
	if jar == "" {
		t.Skip("本地 ~/.m2 没有 log4j-core-2.17.0")
	}
	d := testDecompiler(t)

	out := t.TempDir()
	res, err := d.Decompile(context.Background(), jar, out)
	if err != nil {
		t.Fatalf("反编译失败: %v", err)
	}
	if res.Files == 0 {
		t.Fatal("反编译产出 0 个 .java")
	}
	// Must have fallen back or used the configured engine, and say which.
	if res.Engine != EngineCFR && res.Engine != EngineVineflower {
		t.Errorf("engine = %q, 期望 cfr 或 vineflower", res.Engine)
	}
	jndi := filepath.Join(out, "org", "apache", "logging", "log4j", "core", "lookup", "JndiLookup.java")
	b, err := os.ReadFile(jndi)
	if err != nil {
		t.Fatalf("金标准文件缺失 %s: %v", jndi, err)
	}
	body := string(b)
	if !strings.Contains(body, "package org.apache.logging.log4j.core.lookup;") {
		t.Error("JndiLookup.java 缺少预期的 package 声明")
	}
	if !strings.Contains(strings.ToLower(body), "jndi") {
		t.Error("JndiLookup.java 里看不到 JNDI 相关调用")
	}
	t.Logf("反编译 %s：%d 个 .java / %.1f KB / %v / engine=%s / attempts=%v",
		filepath.Base(jar), res.Files, float64(res.Bytes)/1024, res.Duration.Round(time.Millisecond), res.Engine, res.Attempts)
}

// TestMaterializeDecompilesOnlyNamedJar proves the core cost invariant: a service
// with an ingested jar produces NO .java until that specific jar is materialised.
func TestMaterializeDecompilesOnlyNamedJar(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过")
	}
	jar := findLocalJar(log4jRel)
	if jar == "" {
		t.Skip("本地 ~/.m2 没有 log4j-core-2.17.0")
	}
	d := testDecompiler(t)
	dataDir := t.TempDir()
	svc := NewService(dataDir, d)
	svc.Logf = t.Logf
	const taskID = "42"

	// Stage the jar the way an upload would.
	jarsDir := TaskJarsDir(dataDir, taskID)
	if err := os.MkdirAll(jarsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rel := "org/apache/logging/log4j/log4j-core/2.17.0/log4j-core-2.17.0.jar"
	dst := filepath.Join(jarsDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFileForTest(jar, dst); err != nil {
		t.Fatal(err)
	}

	// --- L0/L1: ingest is cheap and produces no source ---
	infos, err := svc.Ingest(taskID)
	if err != nil {
		t.Fatalf("Ingest 失败: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("Ingest 得到 %d 个 jar, 期望 1", len(infos))
	}
	if got, want := infos[0].GAV.String(), "org.apache.logging.log4j:log4j-core:2.17.0"; got != want {
		t.Errorf("GAV = %q, 期望 %q", got, want)
	}
	if files, _, ok := countJavaAt(TaskSrcDir(dataDir, taskID)); ok {
		t.Fatalf("ingest 之后 src/ 下不应有任何 .java（实际 %d 个）——默认不反编译被破坏", files)
	}

	// --- L2: materialise exactly this one dependency ---
	dep := DepView{
		GAV:        infos[0].GAV,
		JarRelPath: infos[0].RelPath,
		SHA256:     infos[0].SHA256,
		ClassCount: infos[0].ClassCount,
	}
	res, err := svc.Materialize(context.Background(), taskID, dep)
	if err != nil {
		t.Fatalf("Materialize 失败: %v", err)
	}
	if res.State != SourceDecompiled {
		t.Errorf("state = %q, 期望 %q", res.State, SourceDecompiled)
	}
	if res.Files == 0 {
		t.Fatal("materialize 产出 0 个 .java")
	}
	if _, err := os.Stat(filepath.Join(res.AbsPath, "org", "apache", "logging", "log4j", "core", "lookup", "JndiLookup.java")); err != nil {
		t.Errorf("materialize 后 JndiLookup.java 不可读: %v", err)
	}
	// Cache must now hold the tree so a second task pays nothing.
	cacheDir := CacheEntryDir(dataDir, infos[0].SHA256)
	if files, _, ok := countJavaAt(cacheDir); !ok || files == 0 {
		t.Errorf("内容寻址缓存未写入 %s", cacheDir)
	}
	// A second materialize must hit the cache rather than re-run the decompiler.
	before := time.Now()
	res2, err := svc.Materialize(context.Background(), taskID, dep)
	if err != nil {
		t.Fatalf("第二次 Materialize 失败: %v", err)
	}
	if !res2.FromCache {
		t.Error("第二次 materialize 未命中缓存")
	}
	if elapsed := time.Since(before); elapsed > 20*time.Second {
		t.Errorf("缓存命中仍耗时 %v，说明又跑了一次反编译", elapsed)
	}
}

// TestExtractJavaSources verifies the zero-JVM path: a real "-sources.jar" is
// extracted as original source (and is never sent through a decompiler).
func TestExtractJavaSources(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "demo-1.0-sources.jar")
	f, err := os.Create(jarPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	entries := map[string]string{
		"com/example/Demo.java":               "package com.example;\npublic class Demo {}\n",
		"com/example/internal/Helper.java":    "package com.example.internal;\nclass Helper {}\n",
		"META-INF/MANIFEST.MF":                "Manifest-Version: 1.0\n",
		"com/example/logo.png":                "not-a-java-file",
		"../../../escape/Evil.java":           "package escape;\n",
		"/absolute/Abs.java":                  "package abs;\n",
		"com/example/README.md":               "# demo\n",
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
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")
	files, bytes, err := ExtractJavaSources(jarPath, out)
	if err != nil {
		t.Fatalf("ExtractJavaSources 失败: %v", err)
	}
	if files != 2 {
		t.Errorf("提取到 %d 个 .java, 期望 2（png 不算，路径穿越的两个必须被拒绝）", files)
	}
	if bytes == 0 {
		t.Error("字节数为 0")
	}
	if _, err := os.Stat(filepath.Join(out, "com", "example", "Demo.java")); err != nil {
		t.Errorf("Demo.java 未提取: %v", err)
	}
	// Path traversal must not have escaped.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); err == nil {
		t.Error("zip-slip：../../../escape 逃出了目标目录")
	}
	if _, err := os.Stat(filepath.Join(out, "absolute", "Abs.java")); err == nil {
		t.Error("绝对路径条目不应被提取")
	}
	// Non-source files are skipped.
	if _, err := os.Stat(filepath.Join(out, "com", "example", "logo.png")); err == nil {
		t.Error("png 不应被提取")
	}
}

// TestWriteIndexCarriesRedLines guards the three warnings the agent must see.
func TestWriteIndexCarriesRedLines(t *testing.T) {
	var sb strings.Builder
	deps := []DepView{{
		GAV:         GAV{Group: "org.apache.logging.log4j", Artifact: "log4j-core", Version: "2.17.0"},
		Scope:       "compile",
		Direct:      true,
		JarRelPath:  "deps/jars/org/apache/logging/log4j/log4j-core/2.17.0/log4j-core-2.17.0.jar",
		ClassCount:  644,
		Size:        1789339,
		SourceState: SourceNone,
	}, {
		GAV:         GAV{Group: "org.rocksdb", Artifact: "rocksdbjni", Version: "8.8.1"},
		JarRelPath:  "deps/jars/org/rocksdb/rocksdbjni/8.8.1/rocksdbjni-8.8.1.jar",
		NativeHeavy: true,
		Size:        58 << 20,
	}}
	if err := WriteIndex(&sb, "42", deps, "index/classes.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	body := sb.String()
	for _, want := range []string{
		"反编译产物，不是原始源码", // decompiled != original
		"不可信输入",          // untrusted input / prompt injection
		"默认不会反编译任何 jar",   // on-demand by default
		"org.apache.logging.log4j:log4j-core:2.17.0",
		"index/classes.txt",
		"ensure_dependency_source",
		"无 class 文件", // native-heavy jar is called out
	} {
		if !strings.Contains(body, want) {
			t.Errorf("INDEX.md 缺少内容: %q", want)
		}
	}
}

// TestEmbeddedCFRMaterialize verifies the CFR jar compiled into the binary is a
// real, runnable jar and that extracting it is idempotent (no pointless rewrites).
func TestEmbeddedCFRMaterialize(t *testing.T) {
	if !EmbeddedCFRAvailable() {
		t.Skip("构建未内嵌 CFR")
	}
	dir := t.TempDir()
	p, err := MaterializeEmbeddedCFR(dir)
	if err != nil {
		t.Fatalf("MaterializeEmbeddedCFR 失败: %v", err)
	}
	if p != filepath.Join(dir, EmbeddedCFRName) {
		t.Errorf("路径 = %q, 期望 %q", p, filepath.Join(dir, EmbeddedCFRName))
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != int64(len(embeddedCFR)) {
		t.Errorf("大小 = %d, 期望 %d", fi.Size(), len(embeddedCFR))
	}

	// It must really be CFR, not an empty placeholder.
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatalf("内置 jar 不是合法 zip: %v", err)
	}
	var manifest string
	for _, e := range zr.File {
		if e.Name == "META-INF/MANIFEST.MF" {
			rc, err := e.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			manifest = string(b)
		}
	}
	// Close before the rewrite checks: Windows keeps the jar locked otherwise.
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(manifest, "org.benf.cfr.reader.Main") {
		t.Errorf("内置 jar 的 Main-Class 不是 CFR: %q", manifest)
	}

	// Second call must reuse the file rather than rewrite it.
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeEmbeddedCFR(dir); err != nil {
		t.Fatalf("第二次 MaterializeEmbeddedCFR 失败: %v", err)
	}
	again, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ModTime().Equal(old) {
		t.Error("内容一致的内置 jar 被重复写入")
	}

	// A damaged copy must be replaced by the embedded bytes.
	if err := os.WriteFile(p, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeEmbeddedCFR(dir); err != nil {
		t.Fatalf("修复损坏的内置 jar 失败: %v", err)
	}
	if ok, err := embeddedCFRMatches(p); !ok || err != nil {
		t.Errorf("损坏的内置 jar 未被修复（ok=%v err=%v）", ok, err)
	}
}

func copyFileForTest(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
