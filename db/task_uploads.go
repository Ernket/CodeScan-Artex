package db

import (
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// =====================================================================
// 源码包上传与 GitHub 导入（任务创建流程的第一步）
//
// 两种来源产出的东西完全一致：data/uploads/<id>/source 目录 + 一行记录。
// 用户随后创建任务时把这份源码「消费」进该任务的源码目录。
// 磁盘目录是真实数据，表只记录元数据，便于重启后继续使用未消费的上传。
// =====================================================================

// TaskUpload is a row in task_uploads.
type TaskUpload struct {
	ID              string     `json:"id"`
	Filename        string     `json:"filename"`
	StoredPath      string     `json:"-"` // 服务器绝对路径，不对外暴露
	SourcePath      string     `json:"-"` // 解压后的源码根目录，不对外暴露
	FileCount       int64      `json:"file_count"`
	TotalBytes      int64      `json:"total_bytes"`
	CreatedBy       *int64     `json:"created_by,omitempty"`
	ConsumedBy      *int64     `json:"consumed_by,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ConsumedAt      *time.Time `json:"consumed_at,omitempty"`
	Origin          string     `json:"origin"`               // upload | github
	SourceURL       string     `json:"source_url,omitempty"` // 规范化后的仓库地址
	SourceRef       string     `json:"source_ref,omitempty"` // 用户指定的分支/标签，可能是空
	ResolvedCommit  string     `json:"resolved_commit,omitempty"`
	State           string     `json:"state"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	DownloadedBytes int64      `json:"downloaded_bytes"`
	DeclaredBytes   int64      `json:"declared_bytes"`
	SizeConfirmed   bool       `json:"size_confirmed"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

// TaskUploadStore operates on task_uploads.
type TaskUploadStore struct{ db *DB }

// TaskUploads returns the upload store.
func (d *DB) TaskUploads() *TaskUploadStore { return &TaskUploadStore{db: d} }

// ErrTaskUploadNotFound is returned when an upload id is unknown or already consumed.
var ErrTaskUploadNotFound = errors.New("upload not found")

// 状态常量。upload 来源的行直接以 StateReady 落库，跳过全部中间态。
const (
	UploadStateFetching        = "fetching"
	UploadStateAwaitingConfirm = "awaiting_confirm"
	UploadStateExtracting      = "extracting"
	UploadStateReady           = "ready"
	UploadStateFailed          = "failed"
)

// NewTaskUpload is the argument struct for CreateTaskUpload. It used to be seven
// positional parameters; GitHub import adds five more, at which point positional
// calls become unreadable and easy to transpose.
type NewTaskUpload struct {
	ID             string
	Filename       string
	StoredPath     string
	SourcePath     string
	FileCount      int64
	TotalBytes     int64
	CreatedBy      *int64
	Origin         string
	SourceURL      string
	SourceRef      string
	ResolvedCommit string
	State          string
	DeclaredBytes  int64
	SizeConfirmed  bool
}

const taskUploadColumns = `id, filename, stored_path, source_path, file_count, total_bytes,
	created_by, consumed_by, created_at, consumed_at,
	origin, source_url, source_ref, resolved_commit, state, error_message,
	downloaded_bytes, declared_bytes, size_confirmed, finished_at`

func scanTaskUpload(sc interface{ Scan(...any) error }) (*TaskUpload, error) {
	var u TaskUpload
	if err := sc.Scan(&u.ID, &u.Filename, &u.StoredPath, &u.SourcePath, &u.FileCount, &u.TotalBytes,
		&u.CreatedBy, &u.ConsumedBy, &u.CreatedAt, &u.ConsumedAt,
		&u.Origin, &u.SourceURL, &u.SourceRef, &u.ResolvedCommit, &u.State, &u.ErrorMessage,
		&u.DownloadedBytes, &u.DeclaredBytes, &u.SizeConfirmed, &u.FinishedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateTaskUpload records one uploaded or imported source tree.
func (s *TaskUploadStore) CreateTaskUpload(in NewTaskUpload) (*TaskUpload, error) {
	if in.State == "" {
		in.State = UploadStateReady
	}
	if in.Origin == "" {
		in.Origin = "upload"
	}
	_, err := s.db.Exec(`
INSERT INTO task_uploads(id, filename, stored_path, source_path, file_count, total_bytes, created_by,
                         origin, source_url, source_ref, resolved_commit, state, declared_bytes, size_confirmed)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		in.ID, in.Filename, in.StoredPath, in.SourcePath, in.FileCount, in.TotalBytes, in.CreatedBy,
		in.Origin, in.SourceURL, in.SourceRef, in.ResolvedCommit, in.State, in.DeclaredBytes, in.SizeConfirmed)
	if err != nil {
		return nil, err
	}
	return s.GetTaskUpload(in.ID)
}

// IsUniqueViolation reports a PostgreSQL unique-index conflict (23505). The
// active-import quota depends on it: two concurrent imports by one account make
// the second INSERT fail here instead of racing a COUNT query.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// GetTaskUpload loads one upload row.
func (s *TaskUploadStore) GetTaskUpload(id string) (*TaskUpload, error) {
	row := s.db.QueryRow(`SELECT `+taskUploadColumns+` FROM task_uploads WHERE id=$1`, id)
	u, err := scanTaskUpload(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTaskUploadNotFound
		}
		return nil, err
	}
	return u, nil
}

// GetTaskUploadByTaskID returns the upload a task was created from. Consumed rows
// are never deleted, so this is also how the task detail page recovers the
// "which commit was audited" fact long after the import finished.
func (s *TaskUploadStore) GetTaskUploadByTaskID(taskID int64) (*TaskUpload, error) {
	row := s.db.QueryRow(`SELECT `+taskUploadColumns+` FROM task_uploads WHERE consumed_by=$1
		ORDER BY consumed_at DESC NULLS LAST LIMIT 1`, taskID)
	u, err := scanTaskUpload(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTaskUploadNotFound
		}
		return nil, err
	}
	return u, nil
}

// MarkTaskUploadConsumed binds an upload to the task created from it. It refuses to
// consume an upload twice (compare-and-set) so two concurrent creates cannot share
// the same staging directory.
func (s *TaskUploadStore) MarkTaskUploadConsumed(id string, taskID int64) (bool, error) {
	res, err := s.db.Exec(`
UPDATE task_uploads SET consumed_by=$2, consumed_at=now()
WHERE id=$1 AND consumed_by IS NULL`, id, taskID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkTaskUploadAwaitingConfirm parks a finished download that exceeded the soft
// limit. Only a still-fetching row may transition, so a cancel that races with
// the download cannot resurrect the row.
func (s *TaskUploadStore) MarkTaskUploadAwaitingConfirm(id string, bytes int64) (bool, error) {
	res, err := s.db.Exec(`
UPDATE task_uploads SET state=$2, downloaded_bytes=$3
WHERE id=$1 AND state=$4`, id, UploadStateAwaitingConfirm, bytes, UploadStateFetching)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkTaskUploadSizeConfirmed records that the user accepted a large import at the
// second confirmation dialog (the first one is recorded at creation time).
func (s *TaskUploadStore) MarkTaskUploadSizeConfirmed(id string) error {
	_, err := s.db.Exec(`
UPDATE task_uploads SET size_confirmed=true
WHERE id=$1 AND state=$2`, id, UploadStateAwaitingConfirm)
	return err
}

// MarkTaskUploadExtracting accepts both entries of the state machine: straight
// from fetching (under the soft limit, or pre-confirmed) and from awaiting_confirm
// (user accepted).
func (s *TaskUploadStore) MarkTaskUploadExtracting(id string) (bool, error) {
	res, err := s.db.Exec(`
UPDATE task_uploads SET state=$2
WHERE id=$1 AND state IN ($3,$4)`, id, UploadStateExtracting, UploadStateFetching, UploadStateAwaitingConfirm)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// UpdateTaskUploadProgress writes the byte counter, throttled by the caller.
func (s *TaskUploadStore) UpdateTaskUploadProgress(id string, bytes int64) error {
	_, err := s.db.Exec(`
UPDATE task_uploads SET downloaded_bytes=$2 WHERE id=$1 AND state=$3`, id, bytes, UploadStateFetching)
	return err
}

// MarkTaskUploadReady finishes a successful import. downloadedBytes is written
// here as well because the throttled progress writer skips small archives
// entirely, leaving a "ready" row reporting 0 bytes downloaded.
func (s *TaskUploadStore) MarkTaskUploadReady(id string, downloadedBytes, fileCount, totalBytes int64) (bool, error) {
	res, err := s.db.Exec(`
UPDATE task_uploads SET state=$2, downloaded_bytes=$3, file_count=$4, total_bytes=$5, finished_at=now(), error_message=''
WHERE id=$1 AND state IN ($6,$7)`, id, UploadStateReady, downloadedBytes, fileCount, totalBytes,
		UploadStateFetching, UploadStateExtracting)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkTaskUploadFailed records a terminal failure. Ready rows are never downgraded:
// a late goroutine must not overwrite a successful import.
func (s *TaskUploadStore) MarkTaskUploadFailed(id, message string) error {
	if len(message) > 300 {
		message = message[:300]
	}
	_, err := s.db.Exec(`
UPDATE task_uploads SET state=$2, error_message=$3, finished_at=now()
WHERE id=$1 AND state <> $4 AND state <> $2`, id, UploadStateFailed, message, UploadStateReady)
	return err
}

// RecoverInterruptedTaskUploads marks rows left mid-flight by a restart as failed
// and returns their ids so the caller can remove the half-written directories.
//
// Without this, a partial unique index on (created_by) would keep blocking the
// account forever: the stale row still counts as "an import in progress".
func (s *TaskUploadStore) RecoverInterruptedTaskUploads() ([]string, error) {
	rows, err := s.db.Query(`
UPDATE task_uploads
SET state=$1, error_message='服务重启，导入已中断', finished_at=now()
WHERE state IN ($2,$3,$4)
RETURNING id`, UploadStateFailed, UploadStateFetching, UploadStateAwaitingConfirm, UploadStateExtracting)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListOpenTaskUploads lists uploads that were never bound to a task (newest first).
func (s *TaskUploadStore) ListOpenTaskUploads(limit int) ([]*TaskUpload, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+taskUploadColumns+`
FROM task_uploads
WHERE consumed_by IS NULL AND state IN ($1,$2,$3,$4)
ORDER BY created_at DESC LIMIT $5`,
		UploadStateFetching, UploadStateAwaitingConfirm, UploadStateExtracting, UploadStateReady, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TaskUpload{}
	for rows.Next() {
		u, err := scanTaskUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteTaskUpload removes an upload row (its directory is removed by the caller).
func (s *TaskUploadStore) DeleteTaskUpload(id string) error {
	_, err := s.db.Exec(`DELETE FROM task_uploads WHERE id=$1`, id)
	return err
}

// AllTaskUploadIDs returns every id currently in task_uploads, keyed for O(1)
// lookup by the janitor's orphan-directory sweep.
func (s *TaskUploadStore) AllTaskUploadIDs() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT id FROM task_uploads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ExpiredTaskUploads returns ids of rows the janitor may remove.
//
// keepReadyFor is the TTL for unused ready rows (a user may come back to finish
// creating the task). keepFailedFor covers failed rows (only need to survive long
// enough to be read) and awaiting_confirm rows (the background goroutine gives up
// after 10 minutes on its own; this is the safety net for a row whose goroutine
// died with the process, so the partial unique index is released).
func (s *TaskUploadStore) ExpiredTaskUploads(keepReadyFor, keepFailedFor time.Duration) ([]string, error) {
	rows, err := s.db.Query(`
SELECT id FROM task_uploads
WHERE consumed_by IS NULL
  AND ((state=$1 AND created_at < now() - $2::interval)
    OR (state IN ($3,$5) AND created_at < now() - $4::interval))`,
		UploadStateReady, intervalArg(keepReadyFor),
		UploadStateFailed, intervalArg(keepFailedFor),
		UploadStateAwaitingConfirm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func intervalArg(d time.Duration) string {
	sec := int64(d.Seconds())
	if sec < 0 {
		sec = 0
	}
	return time.Duration(sec * int64(time.Second)).String()
}
