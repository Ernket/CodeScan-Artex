package server

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/javadeps"
)

// =====================================================================
// 第三方依赖（Java jar / .m2）——见 docs/java-m2依赖读取与反编译方案.md
//
// 成本模型（整个功能的重点）：
//   - 上传 + 入库（Ingest）：只做清单 + 类名索引，扫 zip 中央目录，不反编译、不需要 JVM。
//     上千个 jar 也只是几秒，这是默认路径。
//   - 反编译（Materialize）：**只对被点名的那一个 jar** 执行，结果落盘并进内容寻址缓存。
//     绝不批量反编译整个 .m2 —— 实测一个 1.7MB 的 log4j-core 就要 40~60 秒。
//
// 与 agent 的关系：本文件产出的 deps/ 目录是**真实文件树**，agent 用现有的
// Read/Grep/Glob/LS 就能读，能力面完全不变（不加工具、不加网络）。
// =====================================================================

// depsServiceOnce caches the javadeps.Service: it is stateless apart from the
// configured decompiler, and re-probing `java -version` per request would be silly.
var (
	depsServiceMu   sync.Mutex
	depsServiceInst *javadeps.Service
	depsServiceKey  string
)

// depsService builds (and caches) the dependency service for this data dir.
//
// Tool resolution order: explicit config/env → jars under <data>/tools → the CFR
// jar compiled into the binary (materialized once under <data>/cache/decompiler)
// → `java` on PATH. When no decompiler is found the service still works for
// ingest; only on-demand decompilation reports "unavailable".
//
// The local Maven repository (java.local_repo / ARTEX_JAVA_LOCAL_REPO) is optional
// and independent of the decompiler: with it configured, a task can pull a jar by
// coordinate instead of requiring an upload.
func (s *Server) depsService() *javadeps.Service {
	dataDir := s.m.dir
	js := config.JavaSettings()

	depsServiceMu.Lock()
	defer depsServiceMu.Unlock()
	key := strings.Join([]string{dataDir, js.JavaPath, js.VineflowerJar, js.CFRJar, js.MaxHeap, strconv.Itoa(js.TimeoutSeconds), js.LocalRepo}, "|")
	if depsServiceInst != nil && depsServiceKey == key {
		return depsServiceInst
	}

	javaPath := javadeps.FindJava(js.JavaPath)
	vineflower, cfr := js.VineflowerJar, js.CFRJar
	if vineflower == "" || cfr == "" {
		// Auto-discover whatever was dropped into <data>/tools.
		dv, dc := javadeps.DiscoverTools(filepath.Join(dataDir, "tools"))
		if vineflower == "" {
			vineflower = dv
		}
		if cfr == "" {
			cfr = dc
		}
	}
	if cfr == "" && javaPath != "" && javadeps.EmbeddedCFRAvailable() {
		// Bundled CFR: extracted once into the data root, then reused as-is.
		if p, err := javadeps.MaterializeEmbeddedCFR(filepath.Join(dataDir, "cache", "decompiler")); err == nil {
			cfr = p
			log.Printf("[javadeps] 使用内置 CFR %s: %s", javadeps.EmbeddedCFRVersion, p)
		} else {
			log.Printf("[javadeps] 内置 CFR 释放失败（将只做索引）: %v", err)
		}
	}
	timeout := javadeps.DefaultTimeout
	if js.TimeoutSeconds > 0 {
		timeout = time.Duration(js.TimeoutSeconds) * time.Second
	}
	svc := javadeps.NewService(dataDir, &javadeps.Decompiler{
		JavaPath:      javaPath,
		VineflowerJar: vineflower,
		CFRJar:        cfr,
		Timeout:       timeout,
		MaxHeap:       js.MaxHeap,
		Logf:          log.Printf,
	})
	svc.Logf = log.Printf
	// NewLocalRepo returns a disabled repo for an empty/missing path, so a typo in the
	// config degrades to "upload only" instead of breaking dependency handling.
	svc.LocalRepo = javadeps.NewLocalRepo(js.LocalRepo)
	if p := svc.LocalRepoPath(); p != "" {
		log.Printf("[javadeps] 本地 Maven 仓库已启用: %s", p)
	} else if strings.TrimSpace(js.LocalRepo) != "" {
		log.Printf("[javadeps] 本地 Maven 仓库路径不可用（已忽略）: %s", js.LocalRepo)
	}

	depsServiceInst, depsServiceKey = svc, key
	return svc
}

// DependencyDTO is one dependency as the frontend sees it.
type DependencyDTO struct {
	ID          int64    `json:"id"`
	GAV         string   `json:"gav"`
	GroupID     string   `json:"group_id"`
	ArtifactID  string   `json:"artifact_id"`
	Version     string   `json:"version"`
	Classifier  string   `json:"classifier,omitempty"`
	GAVSource   string   `json:"gav_source"`
	Scope       string   `json:"scope,omitempty"`
	Direct      bool     `json:"direct"`
	SizeBytes   int64    `json:"size_bytes"`
	ClassCount  int      `json:"class_count"`
	Packages    int      `json:"package_count"`
	MultiRelease bool    `json:"multi_release"`
	Corrupt     bool     `json:"corrupt"`
	NativeHeavy bool     `json:"native_heavy"`
	SourceState string   `json:"source_state"`
	SourcePath  string   `json:"source_path,omitempty"`
	SourceFiles int      `json:"source_files"`
	SourceBytes int64    `json:"source_bytes"`
	Decompiler  string   `json:"decompiler,omitempty"`
	Error       string   `json:"error,omitempty"`
	TopPackages []string `json:"top_packages,omitempty"`
	// CanMaterialize tells the UI whether a "反编译" action makes sense.
	CanMaterialize bool `json:"can_materialize"`
	// MaterializedAt is RFC3339 when source was produced.
	MaterializedAt string `json:"materialized_at,omitempty"`
}

// DependencyListDTO is the dependency panel payload.
type DependencyListDTO struct {
	TaskID string          `json:"task_id"`
	Deps   []DependencyDTO `json:"deps"`
	// Engine describes the active decompilation capability, e.g. "cfr (java 11)"
	// or "none (未找到 java…)". The UI shows this so users know which level is on.
	Engine string `json:"engine"`
	// DecompileAvailable is false when no JRE/decompiler is configured; ingest and
	// the class index still work in that case.
	DecompileAvailable bool `json:"decompile_available"`
	// TotalClasses is the size of the class index (how much is searchable).
	TotalClasses int `json:"total_classes"`
	// Materialized counts dependencies that have readable source right now.
	Materialized int `json:"materialized"`
	// LocalRepo is the configured local Maven repository path ("" when disabled).
	// With it set, coordinates can be imported without uploading anything.
	LocalRepo string `json:"local_repo"`
	// LocalRepoAvailable is true when that path resolved to a readable directory.
	LocalRepoAvailable bool `json:"local_repo_available"`
}

func depToDTO(d *db.Dependency) DependencyDTO {
	out := DependencyDTO{
		ID: d.ID, GAV: d.GAV, GroupID: d.GroupID, ArtifactID: d.Artifact,
		Version: d.Version, Classifier: d.Classifier, GAVSource: d.GAVSource,
		Scope: d.Scope, Direct: d.Direct, SizeBytes: d.SizeBytes,
		ClassCount: d.ClassCount, Packages: d.PackageCount,
		MultiRelease: d.MultiRelease, Corrupt: d.Corrupt, NativeHeavy: d.NativeHeavy,
		SourceState: d.SourceState, SourcePath: d.SourcePath, SourceFiles: d.SourceFiles,
		SourceBytes: d.SourceBytes, Decompiler: d.Decompiler, Error: d.Error,
		TopPackages: d.TopPackages, CanMaterialize: d.CanMaterialize(),
	}
	if d.Materialized != nil {
		out.MaterializedAt = d.Materialized.Format(time.RFC3339)
	}
	return out
}

// GET /api/tasks/{id}/deps — dependency list + engine capability.
func (s *Server) listTaskDeps(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	taskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil {
		writeErr(w, 400, "bad task id")
		return
	}
	rows, err := s.m.PG().Dependencies().ListDependencies(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	svc := s.depsService()
	out := DependencyListDTO{TaskID: t.ID, Deps: make([]DependencyDTO, 0, len(rows))}
	engine := svc.Decompiler.PickEngine()
	out.Engine = svc.Engine()
	out.DecompileAvailable = engine != javadeps.EngineNone
	out.LocalRepo = svc.LocalRepoPath()
	out.LocalRepoAvailable = svc.LocalRepoEnabled()
	for _, d := range rows {
		out.TotalClasses += d.ClassCount
		if d.SourceState != "" && d.SourceState != string(javadeps.SourceNone) {
			out.Materialized++
		}
		out.Deps = append(out.Deps, depToDTO(d))
	}
	writeJSON(w, 200, out)
}

// POST /api/tasks/{id}/deps — multipart/form-data（字段名 file），内容为一个 zip，
// 里面是要审计的依赖 jar（`mvn dependency:copy-dependencies` 的扁平目录，或
// `~/.m2/repository` 的一部分都支持；坐标优先从 jar 内 pom.properties 读取）。
//
// 上传只做「落盘 + 入库」：解压 jar、扫类名索引、写 INDEX.md，**不反编译**。
func (s *Server) uploadTaskDeps(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	if me == nil {
		writeErr(w, 401, "未授权")
		return
	}
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	taskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil {
		writeErr(w, 400, "bad task id")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, "请求不是合法的文件上传")
		return
	}
	svc := s.depsService()
	if err := svc.EnsureDirs(t.ID); err != nil {
		writeErr(w, 500, "创建依赖目录失败: "+err.Error())
		return
	}
	tmpDir := filepath.Join(s.m.dir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		writeErr(w, 500, "创建临时目录失败: "+err.Error())
		return
	}
	tmpZip, err := os.CreateTemp(tmpDir, "deps-*.zip")
	if err != nil {
		writeErr(w, 500, "创建临时文件失败: "+err.Error())
		return
	}
	tmpZipPath := tmpZip.Name()
	defer os.Remove(tmpZipPath)

	var (
		savedName string
		bytes     int64
	)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = tmpZip.Close()
			writeErr(w, 400, "读取上传内容失败: "+err.Error())
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			_ = part.Close()
			continue
		}
		name := sanitizeUploadFilename(part.FileName())
		if !strings.HasSuffix(strings.ToLower(name), ".zip") {
			_ = part.Close()
			_ = tmpZip.Close()
			writeErr(w, 400, "依赖包只支持 .zip（内含 jar）")
			return
		}
		savedName = name
		bytes, err = io.Copy(tmpZip, part)
		_ = part.Close()
		if err != nil {
			_ = tmpZip.Close()
			writeErr(w, 400, "保存上传文件失败: "+err.Error())
			return
		}
		break
	}
	if err := tmpZip.Close(); err != nil {
		writeErr(w, 500, "写入上传文件失败: "+err.Error())
		return
	}
	if savedName == "" {
		writeErr(w, 400, "未收到文件（表单字段名应为 file）")
		return
	}

	jarsDir := javadeps.TaskJarsDir(s.m.dir, t.ID)
	stats, err := extractDepsZip(tmpZipPath, jarsDir)
	if err != nil {
		writeErr(w, 400, "解压依赖包失败: "+err.Error())
		return
	}
	if stats.jars == 0 {
		writeErr(w, 400, "压缩包里没有 .jar 文件")
		return
	}

	res, err := s.ingestTaskDeps(t.ID, taskID, stats)
	if err != nil {
		writeErr(w, 500, "依赖入库失败: "+err.Error())
		return
	}
	res["filename"] = savedName
	res["upload_bytes"] = bytes
	writeJSON(w, 200, res)
}

// POST /api/tasks/{id}/deps/rescan — 重新扫描已上传的 jar（换了解压/新增 jar 后使用）。
// 只重建清单与索引，不反编译。
func (s *Server) rescanTaskDeps(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	taskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil {
		writeErr(w, 400, "bad task id")
		return
	}
	res, err := s.ingestTaskDeps(t.ID, taskID, depsExtractStats{})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

// depsExtractStats counts what an uploaded dependency zip contained.
type depsExtractStats struct {
	jars    int
	files   int
	bytes   int64
	skipped int
}

// extractDepsZip unpacks only .jar entries of an uploaded zip into destDir,
// preserving the archive's internal structure (so a .m2-shaped zip keeps the
// repository layout and coordinates can be derived from paths). Entry names go
// through the same zip-slip guard as source uploads.
func extractDepsZip(zipPath, destDir string) (depsExtractStats, error) {
	var st depsExtractStats
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return st, err
	}
	defer zr.Close()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return st, err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// Refuse symlinks/devices: a dependency pack is untrusted input.
		if f.Mode()&os.ModeSymlink != 0 || f.Mode()&os.ModeDevice != 0 || f.Mode()&os.ModeNamedPipe != 0 {
			st.skipped++
			continue
		}
		rel, ok := safeZipPath(f.Name)
		if !ok {
			st.skipped++
			continue
		}
		if !javadeps.ValidateJarName(rel) {
			st.skipped++
			continue
		}
		if st.files >= maxExtractFiles || st.bytes+int64(f.UncompressedSize64) > maxExtractBytes {
			return st, errors.New("依赖包解压超过上限")
		}
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return st, err
		}
		rc, err := f.Open()
		if err != nil {
			return st, err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			_ = rc.Close()
			return st, err
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxExtractBytes))
		_ = rc.Close()
		closeErr := out.Close()
		if err != nil {
			return st, err
		}
		if closeErr != nil {
			return st, closeErr
		}
		st.jars++
		st.files++
		st.bytes += n
	}
	return st, nil
}

// ingestTaskDeps scans the task's jars, records them, and regenerates the agent
// facing INDEX.md + class index. Cheap by construction: no decompilation happens
// here, so this is safe to run on every upload and on rescan.
func (s *Server) ingestTaskDeps(taskIDStr string, taskID int64, st depsExtractStats) (map[string]any, error) {
	svc := s.depsService()
	if err := svc.EnsureDirs(taskIDStr); err != nil {
		return nil, err
	}
	infos, err := svc.Ingest(taskIDStr)
	if err != nil {
		return nil, err
	}
	indexed, deps, err := s.recordScannedJars(taskIDStr, taskID, infos, nil)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"jars":          len(infos),
		"dependencies":  deps,
		"indexed_class": indexed,
		"skipped_files": st.skipped,
		"engine":        svc.Engine(),
	}, nil
}

// declaredDep says "the task's own pom.xml declares this jar directly", plus the
// scope it declared. Only the pom.xml scan can know this.
type declaredDep struct {
	scope  string
	direct bool
}

// declaredJarMeta maps a TASK-RELATIVE jar path to its pom declaration. Keyed by
// path, not by coordinate, because a jar's embedded pom.properties may spell its
// coordinate differently from the one the caller asked for — the file on disk is the
// one thing both sides agree on.
type declaredJarMeta map[string]declaredDep

// recordScannedJars upserts scan results for every jar of a task and regenerates the
// manifest, the class index and INDEX.md. Returns (indexedClasses, dependencies).
//
// Shared by the upload/rescan path and the local-repository import path so both end
// up with byte-identical bookkeeping. declared may be nil (upload/rescan: nothing is
// known about scopes); when set, the matching rows are written as direct dependencies
// of the declared scope in the same statement — which is what makes it correct for a
// brand-new row and not just for an existing one.
func (s *Server) recordScannedJars(taskIDStr string, taskID int64, infos []javadeps.JarInfo, declared declaredJarMeta) (int, int, error) {
	store := s.m.PG().Dependencies()
	for _, in := range infos {
		if in.GAV.IsZero() {
			// A jar we cannot even name is useless for lookup; keep it visible as a
			// row keyed by file name so the UI can show it instead of silently dropping.
			in.GAV = javadeps.GAV{Artifact: strings.TrimSuffix(filepath.Base(in.RelPath), ".jar")}
		}
		row := &db.Dependency{
			TaskID:       taskID,
			GroupID:      in.GAV.Group,
			Artifact:     in.GAV.Artifact,
			Version:      in.GAV.Version,
			Classifier:   in.GAV.Classifier,
			GAVSource:    string(in.GAVSource),
			JarPath:      in.RelPath,
			JarSHA256:    in.SHA256,
			SizeBytes:    in.Size,
			ClassCount:   in.ClassCount,
			PackageCount: in.PackageCount,
			IsSources:    in.IsSources,
			MultiRelease: in.MultiReleaseEntries > 0,
			Corrupt:      in.Corrupt,
			NativeHeavy:  in.IsNativeHeavy(),
			TopPackages:  in.TopPackages(5),
		}
		if m, ok := declared[filepath.ToSlash(in.RelPath)]; ok {
			row.Scope, row.Direct = m.scope, m.direct
		}
		if _, err := store.UpsertDependency(row); err != nil {
			return 0, 0, err
		}
	}
	rows, err := store.ListDependencies(taskID)
	if err != nil {
		return 0, 0, err
	}
	indexed, err := s.rebuildDepsIndex(taskIDStr, rows, infos)
	if err != nil {
		return 0, 0, err
	}
	return indexed, len(rows), nil
}

// rebuildDepsIndex writes deps/INDEX.md + deps/index/classes.txt from the DB rows.
// The class index comes from a fresh scan (cheap); the markdown mirrors the DB.
func (s *Server) rebuildDepsIndex(taskIDStr string, rows []*db.Dependency, infos []javadeps.JarInfo) (int, error) {
	deps := make([]javadeps.DepView, 0, len(rows))
	for _, r := range rows {
		state := javadeps.SourceState(r.SourceState)
		if state == "" {
			state = javadeps.SourceNone
		}
		deps = append(deps, javadeps.DepView{
			GAV: javadeps.GAV{
				Group: r.GroupID, Artifact: r.Artifact, Version: r.Version, Classifier: r.Classifier,
			},
			GAVSource:    javadeps.GAVSource(r.GAVSource),
			Scope:        r.Scope,
			Direct:       r.Direct,
			JarRelPath:   r.JarPath,
			SHA256:       r.JarSHA256,
			Size:         r.SizeBytes,
			ClassCount:   r.ClassCount,
			Packages:     r.PackageCount,
			IsSources:    r.IsSources,
			MultiRelease: r.MultiRelease,
			Corrupt:      r.Corrupt,
			NativeHeavy:  r.NativeHeavy,
			SourceState:  state,
			SourceRel:    r.SourcePath,
			SourceFiles:  r.SourceFiles,
			SourceBytes:  r.SourceBytes,
			Decompiler:   r.Decompiler,
			TopPackages:  r.TopPackages,
			Err:          r.Error,
		})
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].GAV.Key() < deps[j].GAV.Key() })

	// Class index: keyed by coordinate so a Grep answers "which dependency has X".
	classes := javadeps.ClassIndexFor(infos)
	indexPath := javadeps.TaskIndexPath(s.m.dir, taskIDStr)
	indexed, err := javadeps.WriteClassIndex(indexPath, deps, classes)
	if err != nil {
		return 0, err
	}

	mdPath := javadeps.TaskIndexMDPath(s.m.dir, taskIDStr)
	f, err := os.Create(mdPath)
	if err != nil {
		return indexed, err
	}
	writeErr := javadeps.WriteIndex(f, taskIDStr, deps, filepath.ToSlash(filepath.Join(javadeps.IndexSubdir, "classes.txt")), time.Now())
	closeErr := f.Close()
	if writeErr != nil {
		return indexed, writeErr
	}
	if closeErr != nil {
		return indexed, closeErr
	}

	manifestPath := javadeps.TaskManifestPath(s.m.dir, taskIDStr)
	if err := javadeps.WriteManifest(manifestPath, javadeps.Manifest{
		TaskID: taskIDStr, GeneratedAt: time.Now(), JarCount: len(deps), Deps: deps,
	}); err != nil {
		return indexed, err
	}
	return indexed, nil
}

// POST /api/tasks/{id}/deps/{depID}/materialize — 按需把一个依赖变成可读源码。
//
// 这是唯一会跑反编译器的入口，且一次只处理一个 jar。若该依赖有真正的
// `-sources.jar`，只解压、不反编译（零 JVM 成本）。
func (s *Server) materializeTaskDep(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	taskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil {
		writeErr(w, 400, "bad task id")
		return
	}
	depID, err := strconv.ParseInt(r.PathValue("depID"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad dependency id")
		return
	}
	store := s.m.PG().Dependencies()
	row, err := store.GetDependency(depID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if row == nil || row.TaskID != taskID {
		writeErr(w, 404, "dependency not found")
		return
	}
	if row.Corrupt {
		writeErr(w, 400, "该依赖不是合法的 jar，无法反编译")
		return
	}
	if row.NativeHeavy {
		writeErr(w, 400, "该依赖没有 class 文件（本地库/平台包），无需反编译")
		return
	}

	svc := s.depsService()
	if svc.Decompiler.PickEngine() == javadeps.EngineNone {
		writeErr(w, 503, "当前环境没有可用的反编译器（需要 JRE + Vineflower/CFR jar）: "+svc.Engine())
		return
	}
	view := javadeps.DepView{
		GAV: javadeps.GAV{
			Group: row.GroupID, Artifact: row.Artifact, Version: row.Version, Classifier: row.Classifier,
		},
		JarRelPath:  row.JarPath,
		SHA256:      row.JarSHA256,
		ClassCount:  row.ClassCount,
		NativeHeavy: row.NativeHeavy,
		Corrupt:     row.Corrupt,
		SourceState: javadeps.SourceState(row.SourceState),
	}
	// Decompiling one jar is tens of seconds; keep the request context but give the
	// subprocess its own (longer) timeout inside the decompiler.
	ctx, cancel := context.WithTimeout(r.Context(), svc.Decompiler.TimeoutOr(javadeps.DefaultTimeout)+30*time.Second)
	defer cancel()

	res, err := svc.Materialize(ctx, t.ID, view)
	if err != nil {
		_ = store.MarkMaterialized(row.ID, "", "", 0, 0, "", err.Error())
		writeErr(w, 500, err.Error())
		return
	}
	if err := store.MarkMaterialized(row.ID, string(res.State), res.RelPath, res.Files, res.Bytes, res.Decompiler, ""); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Refresh INDEX.md so the agent's view of "what is readable" stays true.
	if rows, err := store.ListDependencies(taskID); err == nil {
		infos, _ := svc.Ingest(t.ID)
		_, _ = s.rebuildDepsIndex(t.ID, rows, infos)
	}
	updated, _ := store.GetDependency(row.ID)
	writeJSON(w, 200, map[string]any{
		"dependency": depToDTO(updated),
		"from_cache": res.FromCache,
		"files":      res.Files,
		"bytes":      res.Bytes,
		"engine":     res.Decompiler,
		"attempts":   res.Attempts,
		"duration_ms": res.Duration.Milliseconds(),
	})
}

// =====================================================================
// 本地 Maven 仓库导入：POST /api/tasks/{id}/deps/import
//
// 「配一次、所有任务全局可用」的入口。与上传路线的关系：
//   - 上传：用户把 jar 给我（一个内含 jar 的 zip）；
//   - 导入：按【坐标】去服务器本机的 Maven 仓库取那一个 jar。
// 两条路线产出的东西完全一样（deps/jars 下一个真实 jar），所以清单、类名索引、
// 按需反编译全部原样复用；这里同样【只入库、不反编译】。
//
// 坐标有两个来源，可叠加：
//   - gavs：调用方直接给的坐标列表（人粘贴、或脚本产出）；
//   - scan_source=true：读本任务已上传源码里的 pom.xml，取其直接依赖 ——
//     这是为了回答「我该导入哪些」，让用户不必在服务器上装 Maven。
//
// 与「绝不整库 .m2」的关系：一次请求的坐标数有上限，而且每个坐标都是被点名的，
// 仓库永远只按坐标做点查，不存在遍历。
// =====================================================================

const (
	// maxImportCoords caps one import request: on-demand must not silently become
	// "import the whole repository".
	maxImportCoords = 1000
	// maxImportPoms caps how many pom.xml files one scan_source request reads.
	maxImportPoms = 200
	// maxPomBytes caps a single pom.xml read (they are a few KB; this is a bound).
	maxPomBytes = 2 << 20
)

// Per-coordinate outcomes reported to the UI. Kept as plain strings so the frontend
// can switch on them without a shared enum.
const (
	depImportImported   = "imported"
	depImportAlready    = "already"
	depImportNotFound   = "not_found"
	depImportInvalid    = "invalid"
	depImportUnresolved = "unresolved_version"
	depImportError      = "error"
)

type depsImportRequest struct {
	// GAVs are Maven coordinates, one per element.
	GAVs []string `json:"gavs"`
	// ScanSource asks the backend to read the task's uploaded source tree for
	// pom.xml files and take their direct dependencies as coordinates.
	ScanSource bool `json:"scan_source"`
}

// DepsImportEntry is one coordinate's outcome.
type DepsImportEntry struct {
	GAV    string `json:"gav"`
	Status string `json:"status"`
	// JarPath is the TASK-relative path of the imported jar (deps/jars/...).
	JarPath string `json:"jar_path,omitempty"`
	// Sources is true when a real "-sources.jar" came along: source is then readable
	// with zero decompilation cost.
	Sources bool   `json:"sources,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Error   string `json:"error,omitempty"`
}

// DepsImportResult is the whole request's outcome.
type DepsImportResult struct {
	Imported     int               `json:"imported"`
	Already      int               `json:"already_present"`
	NotFound     int               `json:"not_found"`
	Skipped      int               `json:"skipped"`
	Jars         int               `json:"jars"`
	Dependencies int               `json:"dependencies"`
	IndexedClass int               `json:"indexed_class"`
	Engine       string            `json:"engine"`
	LocalRepo    string            `json:"local_repo"`
	ScannedPoms  int               `json:"scanned_poms"`
	Results      []DepsImportEntry `json:"results"`
	Notes        []string          `json:"notes,omitempty"`
}

// importTarget is one coordinate queued for import.
type importTarget struct {
	gav   javadeps.GAV
	scope string
	// declared is true when the coordinate came from the task's own pom.xml, i.e. the
	// project really depends on it directly. A coordinate the user pasted by hand is
	// not necessarily direct.
	declared bool
}

// POST /api/tasks/{id}/deps/import — 按坐标从本地 Maven 仓库把 jar 拉进本任务。
func (s *Server) importTaskDeps(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	if me == nil {
		writeErr(w, 401, "未授权")
		return
	}
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	taskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil {
		writeErr(w, 400, "bad task id")
		return
	}

	var req depsImportRequest
	if derr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); derr != nil && !errors.Is(derr, io.EOF) {
		writeErr(w, 400, "请求体不是合法 JSON: "+derr.Error())
		return
	}

	svc := s.depsService()
	res := DepsImportResult{Results: []DepsImportEntry{}, LocalRepo: svc.LocalRepoPath()}
	if !svc.LocalRepoEnabled() {
		writeErr(w, 503, "未配置本地 Maven 仓库：请把 config.json 的 java.local_repo（或环境变量 ARTEX_JAVA_LOCAL_REPO）指向本机的 .m2/repository 后重启")
		return
	}
	res.Engine = svc.Engine()

	// ---- 收集坐标：显式列表 +（可选）源码里的 pom.xml ----
	var targets []importTarget
	seen := map[string]int{}
	addTarget := func(g javadeps.GAV, scope string, declared bool) {
		key := g.Key() + ":" + g.Classifier
		if i, dup := seen[key]; dup {
			if targets[i].scope == "" {
				targets[i].scope = scope
			}
			// A pom declaration upgrades an entry that was only pasted by hand.
			targets[i].declared = targets[i].declared || declared
			return
		}
		seen[key] = len(targets)
		targets = append(targets, importTarget{gav: g, scope: scope, declared: declared})
	}

	for _, raw := range req.GAVs {
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		g, perr := javadeps.ParseGAV(text)
		if perr != nil {
			res.Skipped++
			res.Results = append(res.Results, DepsImportEntry{GAV: text, Status: depImportInvalid, Error: perr.Error()})
			continue
		}
		// Coordinates are user input on their way to filesystem paths: validate before
		// anything joins them together.
		if verr := g.SafeValidate(); verr != nil {
			res.Skipped++
			res.Results = append(res.Results, DepsImportEntry{GAV: text, Status: depImportInvalid, Error: verr.Error()})
			continue
		}
		addTarget(g, "", false)
	}

	if req.ScanSource {
		s.scanSourcePoms(t, &res, addTarget)
	}

	if len(targets) > maxImportCoords {
		writeErr(w, 400, fmt.Sprintf("一次最多导入 %d 个坐标（本次 %d 个）：请分批导入", maxImportCoords, len(targets)))
		return
	}
	if len(targets) == 0 && len(res.Results) == 0 {
		res.Notes = append(res.Notes, "没有可导入的坐标：请填写坐标，或勾选「扫描源码里的 pom.xml」")
	}

	// ---- 逐个按坐标取件 ----
	store := s.m.PG().Dependencies()
	importedAny := false
	// declaredByPath carries "this jar is a direct, pom-declared dependency" into the
	// upsert below. Keyed by the task-relative jar path rather than by coordinate,
	// because a jar's own pom.properties may spell its coordinate differently from
	// the one the user asked for — the file is the thing we actually know.
	declaredByPath := map[string]declaredDep{}
	for _, tg := range targets {
		entry := DepsImportEntry{GAV: tg.gav.String(), Scope: tg.scope}
		if existing, ferr := store.FindDependency(taskID, tg.gav.Group, tg.gav.Artifact, tg.gav.Version, tg.gav.Classifier); ferr == nil && existing != nil {
			entry.Status = depImportAlready
			entry.JarPath = existing.JarPath
			res.Already++
		} else {
			ir, ierr := svc.ImportFromLocalRepo(t.ID, tg.gav)
			switch {
			case ierr == nil && ir.Linked:
				entry.Status, entry.JarPath, entry.Sources = depImportImported, ir.JarRelPath, ir.Sources
				res.Imported++
				importedAny = true
			case ierr == nil:
				// The file was already in the task (an earlier import under a differently
				// spelled coordinate, or an upload that used repository layout).
				entry.Status, entry.JarPath, entry.Sources = depImportAlready, ir.JarRelPath, ir.Sources
				res.Already++
			case errors.Is(ierr, javadeps.ErrArtifactNotFound):
				entry.Status, entry.Error = depImportNotFound, ierr.Error()
				res.NotFound++
			default:
				entry.Status, entry.Error = depImportError, ierr.Error()
				res.Skipped++
			}
		}
		if tg.declared && entry.JarPath != "" {
			declaredByPath[entry.JarPath] = declaredDep{scope: tg.scope, direct: true}
		}
		res.Results = append(res.Results, entry)
	}

	// ---- 入库 + 重建索引 ----
	// Two reasons to re-ingest: a new jar arrived, or a pom.xml told us something about
	// an existing one (direct/scope) that has to land in the DB *and* in INDEX.md.
	// Doing the metadata inside the upsert — rather than as a follow-up UPDATE — is what
	// makes it correct on INSERT as well as on CONFLICT.
	if importedAny || len(declaredByPath) > 0 {
		infos, ierr := svc.Ingest(t.ID)
		if ierr != nil {
			writeErr(w, 500, "导入后重新扫描失败: "+ierr.Error())
			return
		}
		indexed, deps, rerr := s.recordScannedJars(t.ID, taskID, infos, declaredByPath)
		if rerr != nil {
			writeErr(w, 500, "导入后重建索引失败: "+rerr.Error())
			return
		}
		res.Jars, res.Dependencies, res.IndexedClass = len(infos), deps, indexed
	} else {
		// 没有任何变化时也要回填数字，前端才能直接显示；这里刻意不跑 Ingest
		// （它会重新哈希每个 jar），只用清单和目录计数。
		if rows, lerr := store.ListDependencies(taskID); lerr == nil {
			res.Dependencies = len(rows)
			for _, d := range rows {
				res.IndexedClass += d.ClassCount
			}
		}
		if rels, lerr := javadeps.ListJars(javadeps.TaskJarsDir(s.m.dir, t.ID)); lerr == nil {
			res.Jars = len(rels)
		}
	}

	writeJSON(w, 200, res)
}

// scanSourcePoms reads the task's uploaded source tree for pom.xml files and queues
// their direct dependencies as import targets.
//
// Deliberately best-effort (see javadeps.ParsePomDependencies): whatever cannot be
// resolved is reported as `unresolved_version` with the raw text, never guessed at.
// test-scope dependencies are skipped — they do not ship with the application — and
// the count is reported so the user can add them by hand if they disagree.
func (s *Server) scanSourcePoms(t *Task, res *DepsImportResult, addTarget func(javadeps.GAV, string, bool)) {
	srcRel := strings.TrimSpace(t.SourcePath)
	if srcRel == "" {
		res.Notes = append(res.Notes, "本任务没有上传源码包，跳过 pom.xml 扫描")
		return
	}
	root := filepath.Join(s.m.dir, filepath.FromSlash(srcRel))
	poms := javadeps.FindPoms(root, maxImportPoms)
	res.ScannedPoms = len(poms)
	if len(poms) == 0 {
		res.Notes = append(res.Notes, "源码包里没有找到 pom.xml（非 Maven 工程？可以直接在输入框里粘贴坐标）")
		return
	}
	testScope := 0
	for _, p := range poms {
		body, rerr := readFileCapped(p, maxPomBytes)
		if rerr != nil {
			res.Notes = append(res.Notes, "读取失败: "+filepath.ToSlash(p))
			continue
		}
		rel, rerr := filepath.Rel(s.m.dir, p)
		if rerr != nil {
			rel = p
		}
		deps, perr := javadeps.ParsePomDependencies(body, filepath.ToSlash(rel))
		if perr != nil {
			res.Notes = append(res.Notes, perr.Error())
			continue
		}
		for _, d := range deps {
			if strings.EqualFold(d.Scope, "test") {
				testScope++
				continue
			}
			g, ok := d.GAV()
			if !d.Resolved || !ok {
				res.Skipped++
				res.Results = append(res.Results, DepsImportEntry{
					GAV:    pomDepLabel(d),
					Status: depImportUnresolved,
					Scope:  d.Scope,
					Error:  fmt.Sprintf("版本无法解析（原文 %q）：通常来自 parent/BOM 或外部 property，请手动补全坐标后重试", d.VersionRaw),
				})
				continue
			}
			addTarget(g, d.Scope, true)
		}
	}
	if testScope > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("已跳过 %d 条 test scope 依赖（不参与运行）；需要的话把坐标手动填进输入框", testScope))
	}
}

// pomDepLabel renders a pom declaration for reporting, keeping the raw (possibly
// unresolved) version text so the user can see what the pom actually said.
func pomDepLabel(d javadeps.PomDep) string {
	label := d.Group + ":" + d.Artifact
	switch {
	case d.VersionRaw != "":
		label += ":" + d.VersionRaw
	case d.Version != "":
		label += ":" + d.Version
	}
	if d.Classifier != "" {
		label += ":" + d.Classifier
	}
	return label
}

// readFileCapped reads at most max bytes of a file (a "pom.xml" bigger than that is
// not a pom, and must not be read into memory on a whim).
func readFileCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

// DELETE /api/tasks/{id}/deps — 清空本任务的依赖（DB 行 + deps 目录）。
func (s *Server) deleteTaskDeps(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	taskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil {
		writeErr(w, 400, "bad task id")
		return
	}
	if err := s.m.PG().Dependencies().DeleteTaskDependencies(taskID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := javadeps.RemoveTaskDeps(s.m.dir, t.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
