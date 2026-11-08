package javadeps

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// CFR is compiled into the binary so a fresh install can decompile dependencies
// with only a JRE present. It is a *fallback*, not an override: an explicitly
// configured jar or one dropped into <data>/tools always wins, so a user can swap
// in a newer CFR without rebuilding.
const (
	// EmbeddedCFRVersion is the bundled CFR release.
	EmbeddedCFRVersion = "0.152"
	// EmbeddedCFRName is the file name the bundled jar is written as.
	EmbeddedCFRName = "cfr-" + EmbeddedCFRVersion + ".jar"
)

//go:embed tools/cfr-0.152.jar
var embeddedCFR []byte

var (
	embeddedHashOnce sync.Once
	embeddedHashVal  string
)

// ErrNoEmbeddedCFR is returned when a build shipped without the jar (SOURCE_DATE
// style builds may strip it); callers then simply degrade to "index only".
var ErrNoEmbeddedCFR = errors.New("构建中未内嵌 CFR jar")

// EmbeddedCFRAvailable reports whether a decompiler jar was compiled in.
func EmbeddedCFRAvailable() bool { return len(embeddedCFR) > 0 }

// MaterializeEmbeddedCFR writes the bundled CFR jar into dir and returns its
// path. It is idempotent: when an identical file is already there it only
// stats/hashes it, so the normal path costs no write. The output stays under the
// caller's data root (never a temp dir outside it).
func MaterializeEmbeddedCFR(dir string) (string, error) {
	if !EmbeddedCFRAvailable() {
		return "", ErrNoEmbeddedCFR
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, EmbeddedCFRName)
	if ok, err := embeddedCFRMatches(dst); ok && err == nil {
		return dst, nil
	}

	tmp, err := os.CreateTemp(dir, EmbeddedCFRName+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(embeddedCFR); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		// Windows refuses to replace an existing file; drop the stale copy first.
		if rmErr := os.Remove(dst); rmErr != nil {
			return "", err
		}
		if err := os.Rename(tmpName, dst); err != nil {
			return "", err
		}
	}
	return dst, nil
}

// embeddedCFRMatches reports whether path already holds exactly the embedded jar.
func embeddedCFRMatches(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if fi.Size() != int64(len(embeddedCFR)) {
		return false, nil
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return false, err
	}
	return sum == embeddedDigest(), nil
}

func embeddedDigest() string {
	embeddedHashOnce.Do(func() {
		sum := sha256.Sum256(embeddedCFR)
		embeddedHashVal = hex.EncodeToString(sum[:])
	})
	return embeddedHashVal
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
