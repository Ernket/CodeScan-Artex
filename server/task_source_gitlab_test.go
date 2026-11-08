package server

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/config"
)

// TestUnlimitedImportRemovesCaps 盯住「GitLab 不做体积截断」这个产品决定。
//
// GitLab 的 archive.zip 是 chunked、匿名又拿不到 statistics，体积无法预估；
// 一旦有人把这里的 0/-1 改回策略默认值，GitLab 导入会重新出现「下到一半被
// 硬上限掐断」，而错误文案又说不清原因。这个用例就是那道锁。
func TestUnlimitedImportRemovesCaps(t *testing.T) {
	base := config.GitHubImport{
		MaxZipBytes:         512 << 20,
		HardMaxZipBytes:     4096 << 20,
		MaxExtractBytes:     4096 << 20,
		HardMaxExtractBytes: 12288 << 20,
		MaxFiles:            100000,
	}
	got := unlimitedImport(base)
	if got.MaxZipBytes != 0 || got.HardMaxZipBytes != 0 {
		t.Errorf("下载上限应清零（0=不限制），得到 soft=%d hard=%d", got.MaxZipBytes, got.HardMaxZipBytes)
	}
	if got.MaxExtractBytes != -1 || got.HardMaxExtractBytes != -1 || got.MaxFiles != -1 {
		t.Errorf("解压上限应为 -1（不限制），得到 bytes=%d hard=%d files=%d",
			got.MaxExtractBytes, got.HardMaxExtractBytes, got.MaxFiles)
	}
	// 不能顺手改掉原配置：GitHub 仍然按原值走。
	if base.MaxZipBytes != 512<<20 || base.HardMaxZipBytes != 4096<<20 {
		t.Errorf("unlimitedImport 不应修改入参: %+v", base)
	}
}

// TestExtractZipUnlimited 验证负数限额真的绕过了字节/文件数闸门，而正数仍然生效。
func TestExtractZipUnlimited(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name, body string) {
		t.Helper()
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	// GitLab 的包裹目录形如 project-<sha>-<sha>/，前缀仍是 project-。
	write("project-abc123/src/main.go", "package main\n")
	write("project-abc123/README.md", "# demo\n")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "gitlab.zip")
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// 正数限额：必然失败，确认闸门本身没被改坏。
	dest := filepath.Join(t.TempDir(), "small")
	if _, err := extractZipArchiveOpts(zipPath, dest, extractOptions{
		WrapperPrefix: "project-",
		MaxBytes:      1,
		MaxFiles:      10,
	}); !errors.Is(err, errTooLargeExtract) {
		t.Fatalf("超过 MaxBytes 应报 errTooLargeExtract，得到 %v", err)
	}
	// 负数限额：不限制，应完整解出并剥掉包裹目录。
	dest = filepath.Join(t.TempDir(), "unlimited")
	stats, err := extractZipArchiveOpts(zipPath, dest, extractOptions{
		WrapperPrefix: "project-",
		MaxBytes:      -1,
		MaxFiles:      -1,
	})
	if err != nil {
		t.Fatalf("不限制时应解压成功，得到 %v", err)
	}
	if stats.Files != 2 {
		t.Errorf("应解出 2 个文件，实际 %d", stats.Files)
	}
	if _, err := os.Stat(filepath.Join(dest, "src", "main.go")); err != nil {
		t.Errorf("包裹目录应被剥掉，src/main.go 不存在: %v", err)
	}
}
