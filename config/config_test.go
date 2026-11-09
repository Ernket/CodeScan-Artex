package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPostgresDSNPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(`{"database":{"host":"10.1.2.3","port":6000,"user":"u","password":"p","dbname":"d","sslmode":"require"}}`), 0o644)
	t.Setenv("ARTEX_CONFIG", cfgPath)

	// no env DSN → built from config file fields
	t.Setenv("ARTEX_PG_DSN", "")
	got, _, err := PostgresDSN()
	want := "postgres://u:p@10.1.2.3:6000/d?sslmode=require"
	if err != nil || got != want {
		t.Fatalf("from file: got %q err %v want %q", got, err, want)
	}

	// env wins over config file
	t.Setenv("ARTEX_PG_DSN", "postgres://envwins/x")
	if got, _, err := PostgresDSN(); err != nil || got != "postgres://envwins/x" {
		t.Fatalf("env should win, got %q err %v", got, err)
	}

	// no env, no file → error (no built-in default)
	t.Setenv("ARTEX_PG_DSN", "")
	t.Setenv("ARTEX_CONFIG", filepath.Join(dir, "nope.json"))
	if got, _, err := PostgresDSN(); err == nil {
		t.Fatalf("missing config should error, got %q", got)
	}

	// full dsn in config file is used verbatim
	os.WriteFile(cfgPath, []byte(`{"database":{"dsn":"postgres://full/dsn"}}`), 0o644)
	t.Setenv("ARTEX_CONFIG", cfgPath)
	if got, _, err := PostgresDSN(); err != nil || got != "postgres://full/dsn" {
		t.Fatalf("file dsn verbatim, got %q err %v", got, err)
	}
}

func TestGitHubSettingsExtraHosts(t *testing.T) {
	t.Setenv("ARTEX_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	for _, k := range []string{"ARTEX_GITHUB_EXTRA_HOSTS", "ARTEX_GITHUB_ALLOW_PRIVATE_NETWORK"} {
		t.Setenv(k, "")
	}

	g := GitHubSettings()
	if !g.PrivateNetworkAllowed() {
		t.Error("默认应允许访问内网地址")
	}
	if len(g.ExtraHosts) != 0 {
		t.Errorf("默认不应有自定义 host: %#v", g.ExtraHosts)
	}

	t.Setenv("ARTEX_GITHUB_EXTRA_HOSTS", "gitlab.corp.local, 192.168.1.50")
	t.Setenv("ARTEX_GITHUB_ALLOW_PRIVATE_NETWORK", "0")
	g = GitHubSettings()
	if len(g.ExtraHosts) != 2 || g.ExtraHosts[0] != "gitlab.corp.local" || g.ExtraHosts[1] != "192.168.1.50" {
		t.Errorf("extra hosts 解析不对: %#v", g.ExtraHosts)
	}
	if g.PrivateNetworkAllowed() {
		t.Error("显式 0 应关闭内网访问")
	}
}

// TestGitHubSettingsDefaultsAndEnv 覆盖导入限额的解析与「自愈」规则。
//
// 关键约束：配置写错不能把功能或进程带崩，只能被纠正。
func TestGitHubSettingsDefaultsAndEnv(t *testing.T) {
	t.Setenv("ARTEX_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	for _, k := range []string{"ARTEX_GITHUB_IMPORT", "ARTEX_GITHUB_MAX_MB", "ARTEX_GITHUB_HARD_MAX_MB",
		"ARTEX_GITHUB_MAX_EXTRACT_MB", "ARTEX_GITHUB_TIMEOUT", "ARTEX_GITHUB_CONFIRM_TIMEOUT", "ARTEX_GITHUB_PROXY"} {
		t.Setenv(k, "")
	}

	g := GitHubSettings()
	if g.Disabled {
		t.Error("默认应开启")
	}
	if g.MaxZipBytes != defaultGitHubMaxZipMB<<20 || g.HardMaxZipBytes != defaultGitHubHardMaxZipMB<<20 {
		t.Errorf("默认限额不对: soft=%d hard=%d", g.MaxZipBytes, g.HardMaxZipBytes)
	}
	if g.MaxFiles != defaultGitHubMaxFiles || g.TimeoutSeconds != defaultGitHubTimeoutSeconds ||
		g.ConfirmTimeoutSeconds != defaultGitHubConfirmSeconds {
		t.Errorf("默认阈值不对: %+v", g)
	}
	if g.HardMaxZipBytes < g.MaxZipBytes || g.HardMaxExtractBytes < g.MaxExtractBytes {
		t.Errorf("硬上限必须不小于软上限: %+v", g)
	}

	t.Setenv("ARTEX_GITHUB_MAX_MB", "8")
	t.Setenv("ARTEX_GITHUB_MAX_EXTRACT_MB", "16")
	t.Setenv("ARTEX_GITHUB_TIMEOUT", "90")
	t.Setenv("ARTEX_GITHUB_CONFIRM_TIMEOUT", "30")
	g = GitHubSettings()
	if g.MaxZipBytes != 8<<20 || g.MaxExtractBytes != 16<<20 || g.TimeoutSeconds != 90 || g.ConfirmTimeoutSeconds != 30 {
		t.Errorf("环境变量未生效: %+v", g)
	}

	// 硬上限小于软上限 → 抬到软上限，而不是报错或让闸门形同虚设。
	t.Setenv("ARTEX_GITHUB_HARD_MAX_MB", "2")
	if g = GitHubSettings(); g.HardMaxZipBytes != 8<<20 {
		t.Errorf("硬上限应被抬到软上限，实得 %d", g.HardMaxZipBytes)
	}

	// 非法/零值视为未设置，回落到默认，不能变成「谁都导不进来」。
	t.Setenv("ARTEX_GITHUB_MAX_MB", "0")
	t.Setenv("ARTEX_GITHUB_MAX_MB", "abc")
	if g = GitHubSettings(); g.MaxZipBytes != defaultGitHubMaxZipMB<<20 {
		t.Errorf("非法值应回落默认，实得 %d", g.MaxZipBytes)
	}

	for _, v := range []string{"off", "0", "false", "OFF", "False"} {
		t.Setenv("ARTEX_GITHUB_IMPORT", v)
		if !GitHubSettings().Disabled {
			t.Errorf("%q 应判定为关闭", v)
		}
	}
	t.Setenv("ARTEX_GITHUB_IMPORT", "on")
	if GitHubSettings().Disabled {
		t.Error("on 不该判定为关闭")
	}
}
