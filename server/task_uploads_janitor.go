package server

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

// Upload staging 的回收策略。
//
// 这些目录是「用户开了抽屉、导入完了、然后没建任务就走了」留下的。没有回收，
// 每个仓库的源码会永久留在磁盘上，且 ready 行永远不会被消费。
const (
	uploadJanitorInterval = 30 * time.Minute
	// 未消费的 ready：给用户一天时间回来建任务（前端会把最近一条恢复到抽屉里）。
	uploadKeepReadyFor = 24 * time.Hour
	// failed：只要能撑到用户刷新页面看到原因就够了。awaiting_confirm 不由
	// janitor 回收（见 ExpiredTaskUploads 的注释），它归 goroutine 的确认超时
	// 和启动恢复管。
	uploadKeepFailedFor = time.Hour
	// 无主目录的宽限期。必须明显大于一次导入的时长：目录在数据库行之前创建，
	// 两者之间存在窗口，宽限期太短会把正在下载的目录删掉。
	uploadOrphanGrace = 2 * time.Hour
)

// startUploadJanitor 启动 staging 回收循环：启动先跑一次，之后每 30 分钟一次。
func (s *Server) startUploadJanitor() {
	go func() {
		// 先看 ctx 再看要不要立刻跑一次：测试用已取消的 ctx 构造 Server，
		// 那种情况下不该对真实库动手。
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		s.runUploadJanitor()
		ticker := time.NewTicker(uploadJanitorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				s.runUploadJanitor()
			}
		}
	}()
}

// runUploadJanitor 删过期行 + 对应目录，再扫掉没有行对应的孤儿目录。
func (s *Server) runUploadJanitor() {
	store := s.m.pg.TaskUploads()
	ids, err := store.ExpiredTaskUploads(uploadKeepReadyFor, uploadKeepFailedFor)
	if err != nil {
		log.Printf("[upload-janitor] 查询过期上传失败: %v", err)
		return
	}
	removed := 0
	for _, id := range ids {
		// 先删行再删目录：行是「这个目录还有主」的唯一凭据。反过来只会在删行
		// 失败时留下一行指向空目录，用户点进去必然报错。
		if err := store.DeleteTaskUpload(id); err != nil {
			log.Printf("[upload-janitor] 删除上传 %s 失败: %v", id, err)
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.m.dir, uploadDirName, id)); err != nil {
			log.Printf("[upload-janitor] 删除目录 %s 失败: %v", id, err)
		}
		removed++
	}
	if removed > 0 {
		log.Printf("[upload-janitor] 已回收 %d 个过期上传", removed)
	}
	s.sweepOrphanUploadDirs()
}

// sweepOrphanUploadDirs 处理「目录还在、行没了」的残留（进程在两步之间被杀、
// 手工改库等）。只删 mtime 超过宽限期的目录，避免误伤正在创建的导入。
func (s *Server) sweepOrphanUploadDirs() {
	root := filepath.Join(s.m.dir, uploadDirName)
	ents, err := os.ReadDir(root)
	if err != nil {
		return // 目录不存在是正常的：一次都没导入过
	}
	known, err := s.m.pg.TaskUploads().AllTaskUploadIDs()
	if err != nil {
		log.Printf("[upload-janitor] 读取上传 id 失败: %v", err)
		return
	}
	cutoff := time.Now().Add(-uploadOrphanGrace)
	swept := 0
	for _, e := range ents {
		if !e.IsDir() || known[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			log.Printf("[upload-janitor] 删除孤儿目录 %s 失败: %v", e.Name(), err)
			continue
		}
		swept++
	}
	if swept > 0 {
		log.Printf("[upload-janitor] 已清理 %d 个孤儿目录", swept)
	}
}
