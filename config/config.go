// Package config loads runtime configuration from a JSON file, with environment
// variables taking precedence. Currently it carries the PostgreSQL connection.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Database is the PostgreSQL connection config. Either set DSN directly, or set
// the component fields and a DSN is assembled from them.
type Database struct {
	DSN      string `json:"dsn"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"dbname"`
	SSLMode  string `json:"sslmode"`
}

// JavaDecompiler configures reading and decompiling Java dependencies (see
// docs/java-m2依赖读取与反编译方案.md). Every field is optional: with none set, the
// platform auto-detects `java` on PATH and looks for a decompiler jar under
// <data>/tools. When nothing usable is found, dependency *reading* still works
// (manifest + class index); only on-demand decompilation is unavailable.
type JavaDecompiler struct {
	// JavaPath is the java executable. Empty = search PATH.
	JavaPath string `json:"java_path"`
	// VineflowerJar / CFRJar are decompiler jars. Vineflower needs JRE 17+ and is
	// preferred when available (multithreaded, better modern-Java support); CFR
	// runs on JRE 6+ and is the fallback.
	VineflowerJar string `json:"vineflower_jar"`
	CFRJar        string `json:"cfr_jar"`
	// TimeoutSeconds caps one jar's decompilation. Default 180.
	TimeoutSeconds int `json:"timeout_seconds"`
	// MaxHeap caps the decompiler JVM heap, e.g. "2g". Default 2g.
	MaxHeap string `json:"max_heap"`
	// LocalRepo is a local Maven repository directory (the one that holds
	// <group as dirs>/<artifact>/<version>/... — normally ~/.m2/repository).
	//
	// When set, a task can pull a jar BY COORDINATE from it instead of requiring an
	// upload, which is what makes "configure once, every task benefits" possible.
	// It is a READ-ONLY source: the platform never writes into it, never walks it
	// (a lookup names one coordinate and touches only the files it maps to), and
	// imported jars are hardlinked (or copied) into the task's own deps/jars.
	LocalRepo string `json:"local_repo"`
}

// GitHubImport configures importing a public repository as task source
// (see docs/github-source-import.md). Zero values mean "use the defaults", so an
// absent config block keeps the feature on with sane limits.
type GitHubImport struct {
	// Disabled turns the whole feature off; every import request answers 503.
	Disabled bool `json:"disabled"`
	// MaxZipBytes is the soft limit. Downloading is never interrupted for it:
	// codeload has no Range support, so stopping would mean re-downloading from
	// zero. Instead the user is asked to confirm (before downloading, using the
	// API's size estimate, or after, using the exact byte count).
	MaxZipBytes int64 `json:"max_zip_bytes"`
	// HardMaxZipBytes is never exceeded and cannot be confirmed away.
	HardMaxZipBytes int64 `json:"hard_max_zip_bytes"`
	// MaxExtractBytes applies when the user did not confirm a large import.
	MaxExtractBytes int64 `json:"max_extract_bytes"`
	// HardMaxExtractBytes is the ceiling after a confirmed import. Raising it with
	// the confirmed zip size avoids "downloaded 1.2 GB but failed extracting".
	HardMaxExtractBytes int64 `json:"hard_max_extract_bytes"`
	// MaxFiles caps the entry count of one archive.
	MaxFiles int `json:"max_files"`
	// TimeoutSeconds bounds the download phase only (not the confirm wait).
	TimeoutSeconds int `json:"timeout_seconds"`
	// ConfirmTimeoutSeconds is how long an awaiting_confirm row survives.
	ConfirmTimeoutSeconds int `json:"confirm_timeout_seconds"`
	// Proxy is an explicit proxy for GitHub traffic, empty = direct. The platform
	// global proxy is deliberately not reused: it carries offensive traffic.
	Proxy string `json:"proxy"`
}

// Config is the on-disk config file shape.
type Config struct {
	Database     Database       `json:"database"`
	SkillDir     string         `json:"skill_dir"`
	Java         JavaDecompiler `json:"java"`
	GitHubImport GitHubImport   `json:"github_import"`
}

// 默认限额。软/硬上限分开：软上限只是「问一句」，硬上限才是闸门。
const (
	defaultGitHubMaxZipMB         = 512
	defaultGitHubHardMaxZipMB     = 4096
	defaultGitHubMaxExtractMB     = 4096
	defaultGitHubHardMaxExtractMB = 12288
	defaultGitHubMaxFiles         = 100000
	defaultGitHubTimeoutSeconds   = 600
	defaultGitHubConfirmSeconds   = 600
)

// GitHubSettings resolves the import settings with precedence:
//
//	env (ARTEX_GITHUB_IMPORT / ARTEX_GITHUB_MAX_MB / ARTEX_GITHUB_HARD_MAX_MB /
//	     ARTEX_GITHUB_MAX_EXTRACT_MB / ARTEX_GITHUB_TIMEOUT /
//	     ARTEX_GITHUB_CONFIRM_TIMEOUT / ARTEX_GITHUB_PROXY)
//	  >  config file (github_import.*)
//	  >  defaults
//
// It never fails; impossible combinations are corrected instead of rejected so a
// bad config cannot take the feature (or the process) down.
func GitHubSettings() GitHubImport {
	g := Load().GitHubImport

	if v := strings.TrimSpace(os.Getenv("ARTEX_GITHUB_IMPORT")); v != "" {
		g.Disabled = v == "0" || strings.EqualFold(v, "off") || strings.EqualFold(v, "false")
	}
	if v := strings.TrimSpace(os.Getenv("ARTEX_GITHUB_MAX_MB")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			g.MaxZipBytes = n << 20
		}
	}
	if v := strings.TrimSpace(os.Getenv("ARTEX_GITHUB_HARD_MAX_MB")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			g.HardMaxZipBytes = n << 20
		}
	}
	if v := strings.TrimSpace(os.Getenv("ARTEX_GITHUB_MAX_EXTRACT_MB")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			g.MaxExtractBytes = n << 20
		}
	}
	if v := strings.TrimSpace(os.Getenv("ARTEX_GITHUB_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			g.TimeoutSeconds = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("ARTEX_GITHUB_CONFIRM_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			g.ConfirmTimeoutSeconds = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("ARTEX_GITHUB_PROXY")); v != "" {
		g.Proxy = v
	}

	if g.MaxZipBytes <= 0 {
		g.MaxZipBytes = defaultGitHubMaxZipMB << 20
	}
	// 硬上限必须先落默认值再做「不小于软上限」的纠正。只做纠正是不够的：
	// 配置文件缺失时 HardMaxZipBytes 是 0，会被直接抬成软上限，于是「512 MiB
	// 问一句、4 GiB 才硬拦」退化成两者都是 512 MiB —— 确认弹窗从此再也放不过
	// 任何大仓库（单测 TestGitHubSettingsDefaultsAndEnv 就是照这个坑写的）。
	if g.HardMaxZipBytes <= 0 {
		g.HardMaxZipBytes = defaultGitHubHardMaxZipMB << 20
	}
	if g.HardMaxZipBytes < g.MaxZipBytes {
		g.HardMaxZipBytes = g.MaxZipBytes
	}
	if g.MaxExtractBytes <= 0 {
		g.MaxExtractBytes = defaultGitHubMaxExtractMB << 20
	}
	if g.HardMaxExtractBytes <= 0 {
		g.HardMaxExtractBytes = defaultGitHubHardMaxExtractMB << 20
	}
	if g.HardMaxExtractBytes < g.MaxExtractBytes {
		g.HardMaxExtractBytes = g.MaxExtractBytes
	}
	if g.MaxFiles <= 0 {
		g.MaxFiles = defaultGitHubMaxFiles
	}
	if g.TimeoutSeconds <= 0 {
		g.TimeoutSeconds = defaultGitHubTimeoutSeconds
	}
	if g.ConfirmTimeoutSeconds <= 0 {
		g.ConfirmTimeoutSeconds = defaultGitHubConfirmSeconds
	}
	return g
}

// JavaSettings resolves the Java decompiler settings with precedence:
//
//	env (ARTEX_JAVA_PATH / ARTEX_VINEFLOWER_JAR / ARTEX_CFR_JAR /
//	     ARTEX_DECOMPILE_TIMEOUT / ARTEX_DECOMPILE_MAX_HEAP /
//	     ARTEX_JAVA_LOCAL_REPO)
//	  >  config file (java.*)
//	  >  auto-detection by the caller (java on PATH, jars under <data>/tools)
//
// It never fails: an empty result simply means "let the caller auto-detect".
func JavaSettings() JavaDecompiler {
	j := Load().Java
	if v := strings.TrimSpace(os.Getenv("ARTEX_JAVA_PATH")); v != "" {
		j.JavaPath = v
	}
	if v := strings.TrimSpace(os.Getenv("ARTEX_VINEFLOWER_JAR")); v != "" {
		j.VineflowerJar = v
	}
	if v := strings.TrimSpace(os.Getenv("ARTEX_CFR_JAR")); v != "" {
		j.CFRJar = v
	}
	if v := strings.TrimSpace(os.Getenv("ARTEX_DECOMPILE_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			j.TimeoutSeconds = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("ARTEX_DECOMPILE_MAX_HEAP")); v != "" {
		j.MaxHeap = v
	}
	if v := strings.TrimSpace(os.Getenv("ARTEX_JAVA_LOCAL_REPO")); v != "" {
		j.LocalRepo = v
	}
	return j
}

// BaseDir is the directory that anchors all runtime artifacts (config.json and
// the data/ store). It is the directory the running binary lives in, so a
// distributed executable keeps its files next to itself on any OS (Windows,
// Linux, …) regardless of the working directory it is launched from.
//
// When launched via `go run`, the binary is throwaway: it sits either in a temp
// build dir (cache miss → fresh link) OR straight inside the Go build cache
// (cache hit → run from $GOCACHE/.../...-d). We detect both and fall back to the
// current working directory so dev artifacts (data/, transcripts) and config
// resolve against the project dir, not the throwaway binary's location.
func BaseDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(exe)
	if isGoRunDir(dir) {
		return "." // throwaway `go run` binary → use CWD
	}
	return dir
}

// isGoRunDir reports whether dir is where `go run` parked its executable: under
// the system temp dir (cache miss), or anywhere inside a Go build cache
// (…/go-build/…, the cache-hit case — NOT under os.TempDir(), which is why the
// old temp-only check failed intermittently). In both cases the binary is
// throwaway, so config/data must resolve against the CWD.
func isGoRunDir(dir string) bool {
	if tmp := os.TempDir(); tmp != "" {
		if rel, err := filepath.Rel(tmp, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	for _, seg := range strings.Split(filepath.ToSlash(dir), "/") {
		if seg == "go-build" {
			return true
		}
	}
	return false
}

// Path returns the config file path. Resolution order:
//  1. env ARTEX_CONFIG (explicit override)
//  2. ./config.json in the current working directory (running from the project
//     dir — robust no matter where `go run` placed the temp/cached binary)
//  3. config.json next to the executable (a distributed binary keeps it beside)
//
// The first existing file wins. If none exist, the CWD path is returned so the
// "not found" message points at the project dir the user most likely expected.
func Path() string {
	if v := strings.TrimSpace(os.Getenv("ARTEX_CONFIG")); v != "" {
		return v
	}
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "config.json"))
	}
	candidates = append(candidates, filepath.Join(BaseDir(), "config.json"))
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

// Load reads and parses the config file. A missing/unreadable file yields a zero
// Config (so callers fall back to defaults) rather than an error.
func Load() Config {
	var c Config
	b, err := os.ReadFile(Path())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

// SkillDir returns the skill root directory with precedence:
//
//	env ARTEX_SKILL_DIR  >  config file (skill_dir)  >  BaseDir()/skills
//
// The directory is created if it does not exist.
func SkillDir() string {
	var d string
	if v := strings.TrimSpace(os.Getenv("ARTEX_SKILL_DIR")); v != "" {
		d = v
	} else if v := strings.TrimSpace(Load().SkillDir); v != "" {
		d = v
	} else {
		d = filepath.Join(BaseDir(), "skills")
	}
	_ = os.MkdirAll(d, 0o755)
	return d
}

// PostgresDSN resolves the connection string with precedence:
//
//	env ARTEX_PG_DSN  >  config file (database.dsn, or assembled from fields)
//
// There is NO built-in fallback: when neither source supplies a database config,
// it returns an error naming the config path it inspected, so startup fails loudly
// instead of silently connecting to a wrong default. source describes where the
// DSN came from (for startup logging).
func PostgresDSN() (dsn, source string, err error) {
	if v := strings.TrimSpace(os.Getenv("ARTEX_PG_DSN")); v != "" {
		return v, "环境变量 ARTEX_PG_DSN", nil
	}
	db := Load().Database
	if d := strings.TrimSpace(db.DSN); d != "" {
		return d, "配置文件 " + Path() + " (database.dsn)", nil
	}
	if db.Host != "" || db.DBName != "" || db.User != "" {
		return db.buildDSN(), "配置文件 " + Path() + " (database 字段)", nil
	}
	return "", "", fmt.Errorf("未找到数据库配置：环境变量 ARTEX_PG_DSN 未设置，且配置文件 %s 未提供 database（dsn 或 host/user/dbname）。请创建该配置文件或设置环境变量后重试", Path())
}

func (d Database) buildDSN() string {
	host := d.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := d.Port
	if port == 0 {
		port = 5432
	}
	ssl := d.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	u := url.URL{
		Scheme: "postgres",
		Host:   host + ":" + strconv.Itoa(port),
		Path:   "/" + d.DBName,
	}
	if d.User != "" {
		if d.Password != "" {
			u.User = url.UserPassword(d.User, d.Password)
		} else {
			u.User = url.User(d.User)
		}
	}
	u.RawQuery = url.Values{"sslmode": {ssl}}.Encode()
	return u.String()
}

// String is a redacted view of the resolved DSN (password masked) for logging.
func Redact(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	if u.User != nil {
		if _, hasPw := u.User.Password(); hasPw {
			u.User = url.UserPassword(u.User.Username(), "****")
		}
	}
	return fmt.Sprintf("%s", u.String())
}
