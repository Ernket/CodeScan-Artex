package server

import (
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
	"strings"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/githubsrc"
)

// =====================================================================
// GitHub 仓库导入（任务创建流程的另一个入口）
//
// 设计见 docs/github-source-import.md。要点：导入产出与「上传 zip」完全相同的
// staging 目录（data/uploads/<id>/source + 一行 task_uploads），因此 createTask /
// attachUploadToTask / agent 提示词全都不需要改动。
//
// 为什么必须异步：codeload 不返回 Content-Length，实测 7.86 MB 的包在慢链路上
// 要下 2 分 50 秒，同步 POST 会被任何默认 nginx（proxy_read_timeout 60s）掐成 504。
// =====================================================================

var (
	// errCancelled 表示用户主动放弃（确认框点取消，或显式 DELETE）。这不是失败，
	// 清理时删行删目录，不写 failed。
	errCancelled = errors.New("已取消")
	// errConfirmTimeout 表示等待用户确认超时。
	errConfirmTimeout = errors.New("确认超时")
	// errTooLargeHard 表示实际大小越过硬上限。硬上限不能被用户点掉。
	errTooLargeHard = errors.New("超过硬上限")
)

// githubImportJob is the handle the HTTP layer uses to steer one running import.
type githubImportJob struct {
	cancel context.CancelFunc
	done   chan struct{}
	// confirm carries the user's decision while the goroutine waits in
	// awaiting_confirm. Buffered so a double click cannot block the handler.
	confirm chan bool
	// cancelled distinguishes "user asked to stop" from "the process is
	// shutting down" — only the former deletes the row instead of marking it failed.
	cancelled atomic.Bool
}

// githubClient returns the process-wide GitHub client. It is built lazily so a
// Server constructed by tests (or before config is read) still works.
func (s *Server) githubClient() *githubsrc.Client {
	s.githubMu.Lock()
	defer s.githubMu.Unlock()
	if s.githubSrc == nil {
		s.githubSrc = githubsrc.NewClient(config.GitHubSettings().Proxy)
	}
	return s.githubSrc
}

// importGithubReq is the body of POST /api/tasks/import/github.
type importGithubReq struct {
	RepoURL      string `json:"repo_url"`
	Ref          string `json:"ref"`
	ConfirmLarge bool   `json:"confirm_large"`
}

// POST /api/tasks/import/github — 校验 + 估算 + 建行 + 起后台任务，立即返回 202。
//
// 响应有两种形态：
//
//	{needs_confirm:true, estimated_bytes, limit_bytes, hard_limit_bytes, estimate_exceeds_hard}
//	{upload_id, state:"fetching", repo, ref}
//
// 用 202 而不是 409：前端的 http() 封装只把 error 字段抛成异常，结构化错误体拿不到，
// 2xx + 判别字段可以零改动复用。
func (s *Server) importTaskSourceGithub(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	if me == nil {
		writeErr(w, 401, "未授权")
		return
	}
	if ok, err := s.canCreateTask(me); err != nil {
		writeErr(w, 500, err.Error())
		return
	} else if !ok {
		writeErr(w, 403, "当前账号没有创建任务的权限（需要是某个部门的所有者，或管理员）")
		return
	}

	cfg := config.GitHubSettings()
	if cfg.Disabled {
		writeErr(w, 503, "GitHub 导入功能已关闭")
		return
	}

	var req importGithubReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, 400, "请求格式不正确")
		return
	}
	sp, err := githubsrc.ParseRepoURL(req.RepoURL)
	if err != nil {
		writeErr(w, 400, importGithubMessage(err))
		return
	}
	if strings.TrimSpace(req.Ref) != "" {
		ref, err := githubsrc.NormalizeRef(req.Ref)
		if err != nil {
			writeErr(w, 400, importGithubMessage(err))
			return
		}
		sp.Ref = ref
	}

	// 元数据查询是同步的：只有两次小请求，不传大文件，放在这里可以避免
	// 「先建行、再发现要确认」的脏状态。
	metaCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	meta, err := s.githubClient().Repo(metaCtx, sp)
	if err != nil {
		log.Printf("[github] %s 元数据查询失败: %v", sp.String(), err)
		writeErr(w, statusForImportError(err), importGithubMessage(err))
		return
	}

	estimate := meta.SizeKB * 1024
	if estimate < 0 {
		estimate = 0
	}
	// 估算超过硬上限时**不拒**，只多给前端一个标记。
	//
	// `size` 是打包后的 git 仓库体积，不等于 zipball：实测 ARTEX 高估 31%、
	// ernket.github.io 低估 23%（zip 比 size 大）。高估方向下按估算硬拒，会把
	// 本来能导的仓库挡在门外；而低估方向下拦也拦不住。真正的闸门只能是下载时的
	// 精确字节数（见 errTooLargeHard），估算只负责「问一句」。
	if estimate > cfg.MaxZipBytes && !req.ConfirmLarge {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"needs_confirm":         true,
			"repo":                  sp.String(),
			"ref":                   sp.Ref,
			"estimated_bytes":       estimate,
			"limit_bytes":           cfg.MaxZipBytes,
			"hard_limit_bytes":      cfg.HardMaxZipBytes,
			"estimate_exceeds_hard": estimate > cfg.HardMaxZipBytes,
		})
		return
	}

	// 无论 ref 是分支名、tag 还是 40 位 SHA，都走一次 commit 解析。
	//
	// 曾经为省一次 API 配额而让 SHA 直通（IsCommitSHA 即跳过），但运行时测试暴露
	// 了后果：随手编一个全 0 的 SHA 会被 202 接受，用户要等到后台下载 404 才看到
	// 一条「仓库不存在」——既慢又指错方向。解析这一步同时完成了「该 commit 属于
	// 这个仓库」的校验（不属于时 GitHub 返回 422），多花的一次配额是值得的。
	commit := ""
	cctx, ccancel := context.WithTimeout(r.Context(), 20*time.Second)
	commit, err = s.githubClient().ResolveCommit(cctx, sp)
	ccancel()
	if err != nil {
		log.Printf("[github] %s 解析 commit 失败: %v", sp.String(), err)
		writeErr(w, statusForImportError(err), importGithubMessage(err))
		return
	}

	uploadID := newUploadID()
	dir := filepath.Join(s.m.dir, uploadDirName, uploadID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, 500, "创建导入目录失败: "+err.Error())
		return
	}
	sourceDir := filepath.Join(dir, "source")
	createdBy := me.ID
	filename := fmt.Sprintf("%s/%s@%s", sp.Owner, sp.Repo, shortSHA(commit))

	up, err := s.m.pg.TaskUploads().CreateTaskUpload(db.NewTaskUpload{
		ID:             uploadID,
		Filename:       filename,
		StoredPath:     "", // 不保留原始归档（解压后即删）
		SourcePath:     sourceDir,
		CreatedBy:      &createdBy,
		Origin:         "github",
		SourceURL:      "https://github.com/" + sp.String(),
		SourceRef:      sp.Ref,
		ResolvedCommit: commit,
		State:          db.UploadStateFetching,
		DeclaredBytes:  estimate,
		SizeConfirmed:  req.ConfirmLarge,
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		if db.IsUniqueViolation(err) {
			writeErr(w, 429, "已有一次导入正在进行中，请先等待或取消它")
			return
		}
		writeErr(w, 500, "记录导入任务失败: "+err.Error())
		return
	}

	ctx, cancelJob := context.WithCancel(s.ctx)
	job := &githubImportJob{
		cancel:  cancelJob,
		done:    make(chan struct{}),
		confirm: make(chan bool, 1),
	}
	s.importJobs.Store(uploadID, job)
	go s.runGithubImport(ctx, uploadID, sp, commit, meta.Name+"-", req.ConfirmLarge, cfg, job)

	log.Printf("[github] %s ← %s@%s（估算 %.1f MB）", uploadID, sp.String(), shortSHA(commit), float64(estimate)/(1<<20))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"upload_id":  up.ID,
		"state":      up.State,
		"repo":       sp.String(),
		"ref":        sp.Ref,
		"created_at": up.CreatedAt,
	})
}

// GET /api/task-uploads/{id} — 轮询导入状态。
//
// 路径用顶层字面量 task-uploads：写成 /api/tasks/uploads/{id} 会与
// /api/tasks/{id}/coverage 构成二义模式，注册时 panic（见 docs §5.5）。
func (s *Server) getTaskUpload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, 400, "无效的上传 id")
		return
	}
	up, err := s.m.pg.TaskUploads().GetTaskUpload(id)
	if err != nil {
		writeErr(w, 404, "上传不存在或已被使用")
		return
	}
	me := currentUser(r)
	if me != nil && !me.IsAdmin && up.CreatedBy != nil && *up.CreatedBy != me.ID {
		writeErr(w, 403, "无权查看他人的上传")
		return
	}
	writeJSON(w, 200, s.taskUploadView(up))
}

// POST /api/task-uploads/{id}/confirm — 下载完成但超过软上限时，由用户决定是否解压。
func (s *Server) confirmTaskUpload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, 400, "无效的上传 id")
		return
	}
	up, err := s.m.pg.TaskUploads().GetTaskUpload(id)
	if err != nil {
		writeErr(w, 404, "上传不存在或已被使用")
		return
	}
	me := currentUser(r)
	if me != nil && !me.IsAdmin && up.CreatedBy != nil && *up.CreatedBy != me.ID {
		writeErr(w, 403, "无权操作他人的上传")
		return
	}
	if up.State != db.UploadStateAwaitingConfirm {
		writeErr(w, 409, "当前状态不需要确认")
		return
	}
	var req struct {
		Accept bool `json:"accept"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil {
		writeErr(w, 400, "请求格式不正确")
		return
	}
	v, ok := s.importJobs.Load(id)
	if !ok {
		// 进程重启后 job 已不存在（启动恢复会把行标成 failed）。
		writeErr(w, 409, "导入已结束或服务已重启，请重新发起")
		return
	}
	job := v.(*githubImportJob)
	select {
	case job.confirm <- req.Accept:
	default:
		// 已经有决定在队列里，重复点击丢弃即可。
	}
	state := db.UploadStateExtracting
	if !req.Accept {
		state = "cancelled"
	}
	writeJSON(w, 200, map[string]any{"state": state})
}

// runGithubImport 是后台任务本体。
//
// 必须用具名返回值：defer 里读的是这个 err，匿名返回值会让所有失败都被当成成功。
func (s *Server) runGithubImport(ctx context.Context, id string, sp githubsrc.Spec, commit, wrapperPrefix string, sizeConfirmed bool, cfg config.GitHubImport, job *githubImportJob) (err error) {
	dir := filepath.Join(s.m.dir, uploadDirName, id)
	zipPath := filepath.Join(dir, "download.zip")
	sourceDir := filepath.Join(dir, "source")

	defer func() {
		s.finishGithubImport(id, err)
		s.importJobs.Delete(id)
		close(job.done)
	}()

	// 下载单独计时：确认窗口不能和下载共用一份预算，否则下得越久等得越短。
	dlCtx, dlCancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer dlCancel()

	body, _, err := s.githubClient().OpenArchive(dlCtx, sp, commit)
	if err != nil {
		return err
	}
	defer body.Close()

	f, err := os.Create(zipPath)
	if err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	pw := &uploadProgressWriter{f: f, id: id, st: s.m.pg.TaskUploads(), last: time.Now()}
	n, copyErr := io.Copy(pw, io.LimitReader(body, cfg.HardMaxZipBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		if dlCtx.Err() != nil && ctx.Err() == nil {
			return fmt.Errorf("%w（%s），请检查网络或改用更小的仓库", context.DeadlineExceeded, humanSeconds(cfg.TimeoutSeconds))
		}
		return fmt.Errorf("下载中断: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("写入临时文件失败: %w", closeErr)
	}
	if n == 0 {
		return fmt.Errorf("%w：下载内容为空", errNotZip)
	}
	if n > cfg.HardMaxZipBytes {
		// 硬上限不弹窗：它是防止误操作撑爆磁盘的底线，能被点掉就等于没有上限。
		return fmt.Errorf("%w：实际大小超过 %d MiB，导入已终止", errTooLargeHard, cfg.HardMaxZipBytes>>20)
	}

	if n > cfg.MaxZipBytes && !sizeConfirmed {
		ok, mErr := s.m.pg.TaskUploads().MarkTaskUploadAwaitingConfirm(id, n)
		if mErr != nil {
			return mErr
		}
		if !ok {
			return errCancelled // 期间被 DELETE 掉了
		}
		select {
		case accept := <-job.confirm:
			if !accept {
				return errCancelled
			}
			sizeConfirmed = true
			_ = s.m.pg.TaskUploads().MarkTaskUploadSizeConfirmed(id)
		case <-time.After(time.Duration(cfg.ConfirmTimeoutSeconds) * time.Second):
			return fmt.Errorf("%w（%s），导入已取消", errConfirmTimeout, humanSeconds(cfg.ConfirmTimeoutSeconds))
		case <-ctx.Done():
			if job.cancelled.Load() {
				return errCancelled
			}
			return ctx.Err()
		}
	}

	if _, mErr := s.m.pg.TaskUploads().MarkTaskUploadExtracting(id); mErr != nil {
		return mErr
	}

	// 确认过的包允许解压到更大：否则「下了 1.2 GB 却因为解压超 4 GiB 失败」，
	// 等于白花一次带宽。
	extractCap := cfg.MaxExtractBytes
	if sizeConfirmed {
		if cap := 3 * n; cap < cfg.HardMaxExtractBytes && cap > extractCap {
			extractCap = cap
		} else if cap >= cfg.HardMaxExtractBytes {
			extractCap = cfg.HardMaxExtractBytes
		}
	}

	stats, exErr := extractZipArchiveOpts(zipPath, sourceDir, extractOptions{
		WrapperPrefix: wrapperPrefix,
		MaxBytes:      extractCap,
		MaxFiles:      cfg.MaxFiles,
		Context:       ctx,
	})
	if exErr != nil {
		return exErr
	}
	if stats.Files == 0 {
		return fmt.Errorf("%w：压缩包内没有可解压的文件", errNotZip)
	}
	if _, mErr := s.m.pg.TaskUploads().MarkTaskUploadReady(id, n, stats.Files, stats.Bytes); mErr != nil {
		return mErr
	}
	log.Printf("[github] %s 就绪：%s@%s（%d 个文件 / %.1f MB）", id, sp.String(), shortSHA(commit),
		stats.Files, float64(stats.Bytes)/(1<<20))
	return nil
}

// finishGithubImport 是三条终止路径的唯一收口。
func (s *Server) finishGithubImport(id string, err error) {
	dir := filepath.Join(s.m.dir, uploadDirName, id)
	switch {
	case err == nil:
		// 不保留原始归档：staging 里只留 source/。
		_ = os.Remove(filepath.Join(dir, "download.zip"))
	case errors.Is(err, errCancelled):
		// 用户主动放弃：删干净，不记 failed（否则刷新后看到一条莫名的红条）。
		_ = s.m.pg.TaskUploads().DeleteTaskUpload(id)
		_ = os.RemoveAll(dir)
	default:
		msg := importGithubMessage(err)
		_ = s.m.pg.TaskUploads().MarkTaskUploadFailed(id, msg)
		_ = os.RemoveAll(dir) // source/ 可能是半成品
		log.Printf("[github] %s 失败: %v", id, err)
	}
}

// uploadProgressWriter 落盘的同时每 2 秒回写一次进度。
type uploadProgressWriter struct {
	f    *os.File
	n    int64
	last time.Time
	id   string
	st   *db.TaskUploadStore
}

func (w *uploadProgressWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.n += int64(n)
	if time.Since(w.last) >= 2*time.Second {
		w.last = time.Now()
		_ = w.st.UpdateTaskUploadProgress(w.id, w.n)
	}
	return n, err
}

// taskUploadView 是导入状态的对外视图（不含服务器路径）。
func (s *Server) taskUploadView(up *db.TaskUpload) map[string]any {
	v := map[string]any{
		"upload_id":        up.ID,
		"filename":         up.Filename,
		"origin":           up.Origin,
		"state":            up.State,
		"source_url":       up.SourceURL,
		"source_ref":       up.SourceRef,
		"resolved_commit":  up.ResolvedCommit,
		"file_count":       up.FileCount,
		"total_bytes":      up.TotalBytes,
		"downloaded_bytes": up.DownloadedBytes,
		"declared_bytes":   up.DeclaredBytes,
		"size_confirmed":   up.SizeConfirmed,
		"error_message":    up.ErrorMessage,
		"created_at":       up.CreatedAt,
	}
	if up.State == db.UploadStateReady {
		v["entries"] = topLevelEntries(up.SourcePath)
	}
	return v
}

// topLevelEntries 返回源码目录的顶层条目（最多 20 个），供前端确认导入的是哪个仓库。
func topLevelEntries(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// recoverInterruptedGithubImports 在启动时收尾上一次进程留下的半成品。
//
// 不做这一步，残留的 fetching 行会永久占用 (created_by) 上的部分唯一索引，
// 该账号之后每次导入都拿到 429。
func (s *Server) recoverInterruptedGithubImports() {
	ids, err := s.m.pg.TaskUploads().RecoverInterruptedTaskUploads()
	if err != nil {
		log.Printf("[github] 恢复中断的导入失败: %v", err)
		return
	}
	for _, id := range ids {
		_ = os.RemoveAll(filepath.Join(s.m.dir, uploadDirName, id))
	}
	if len(ids) > 0 {
		log.Printf("[github] 已清理 %d 个中断的导入", len(ids))
	}
}

// humanSeconds 把秒数说成人话：整除 60 用分钟，否则用秒。
//
// 不做这个分支就会写出「确认超时（0 分钟）」—— 阈值配成 30 秒时，除法把
// 真正的原因藏起来了。
func humanSeconds(sec int) string {
	if sec >= 60 && sec%60 == 0 {
		return fmt.Sprintf("%d 分钟", sec/60)
	}
	if sec > 60 {
		return fmt.Sprintf("%d 分 %d 秒", sec/60, sec%60)
	}
	return fmt.Sprintf("%d 秒", sec)
}

func shortSHA(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

// importGithubMessage 把内部错误翻成用户能读的中文。绝不透传 err.Error()：
// 里面可能带完整 URL 或本机路径。
func importGithubMessage(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errCancelled):
		return "已取消"
	case errors.Is(err, githubsrc.ErrInvalidURL):
		return err.Error()
	case errors.Is(err, githubsrc.ErrRepoNotFound):
		return "仓库不存在，或为私有仓库（当前版本只支持公开仓库）"
	case errors.Is(err, githubsrc.ErrRefNotFound):
		return "该分支 / 标签 / commit 不存在"
	case errors.Is(err, githubsrc.ErrEmptyRepo):
		return "仓库为空，没有可审计的源码"
	case errors.Is(err, githubsrc.ErrRepoBlocked):
		return "该仓库已被 GitHub 以版权/合规原因屏蔽（HTTP 451），无法导入"
	case errors.Is(err, githubsrc.ErrRateLimited):
		return "GitHub 接口限流（未登录每小时 60 次），请稍后重试"
	case errors.Is(err, context.DeadlineExceeded):
		return "拉取超时，请检查网络或改用更小的仓库"
	case errors.Is(err, context.Canceled):
		return "已取消"
	case errors.Is(err, errTooLargeHard), errors.Is(err, errConfirmTimeout), errors.Is(err, errNotZip),
		errors.Is(err, errTooManyFiles), errors.Is(err, errTooLargeExtract):
		return err.Error()
	default:
		return "拉取失败，请稍后重试（详情见服务端日志）"
	}
}

// statusForImportError 决定同步阶段（元数据 / commit 解析）的 HTTP 状态码。
func statusForImportError(err error) int {
	switch {
	case errors.Is(err, githubsrc.ErrInvalidURL):
		return 400
	case errors.Is(err, githubsrc.ErrRepoNotFound), errors.Is(err, githubsrc.ErrRefNotFound),
		errors.Is(err, githubsrc.ErrEmptyRepo), errors.Is(err, githubsrc.ErrRepoBlocked):
		return 400
	case errors.Is(err, githubsrc.ErrRateLimited):
		return 429
	case errors.Is(err, context.DeadlineExceeded):
		return 504
	default:
		return 502
	}
}
