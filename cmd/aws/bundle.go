package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// prepareAws returns AWS CLI binary and library dirs, ensuring they are ready.
func prepareAws(ctx context.Context) (awsBin, glibcDir, distDir string, _ error) {
	return ensureFromBundle(ctx)
}

// ensureFromBundle prepares AWS CLI from the prebuilt tar.gz bundle.
func ensureFromBundle(ctx context.Context) (awsBin, glibcDir, distDir string, _ error) {
	depsRoot, err := depsDir()
	if err != nil {
		return "", "", "", err
	}
	bundleRoot := filepath.Join(depsRoot, "bundle")
	distDir = filepath.Join(bundleRoot, "awscli", "dist")
	glibcDir = filepath.Join(bundleRoot, "glibc")
	awsBin = filepath.Join(distDir, "aws")

	// Already prepared?
	if isExecutable(awsBin) {
		if _, err := os.Stat(glibcDir); err == nil {
			return awsBin, glibcDir, distDir, nil
		}
	}

	if err := os.MkdirAll(bundleRoot, 0o755); err != nil {
		return "", "", "", err
	}

	arch := runtime.GOARCH
	url := os.Getenv("AWSCLI_TARBALL_URL_" + strings.ToUpper(arch))
	if url == "" {
		url = defaultBundleURL[arch]
	}
	if url == "" {
		return "", "", "", fmt.Errorf(
			"no bundle url configured for arch %q (set AWSCLI_TARBALL_URL_%s)",
			arch, strings.ToUpper(arch),
		)
	}

	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("awsbundle-%d.tar.gz", time.Now().UnixNano()))
	if err := httpGetToFile(ctx, url, tmp); err != nil {
		return "", "", "", fmt.Errorf("download bundle: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()

	if err := untarGzSafe(tmp, bundleRoot); err != nil {
		return "", "", "", fmt.Errorf("extract bundle: %w", err)
	}

	_ = os.Chmod(awsBin, 0o755)
	for _, ld := range []string{
		filepath.Join(glibcDir, "ld-linux-x86-64.so.2"),
		filepath.Join(glibcDir, "ld-linux-aarch64.so.1"),
	} {
		if _, err := os.Stat(ld); err == nil {
			_ = os.Chmod(ld, 0o755)
		}
	}
	return awsBin, glibcDir, distDir, nil
}

// untarGzSafe extracts tar.gz safely with size/path checks.
func untarGzSafe(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var extracted int64

	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		switch h.Typeflag {
		case tar.TypeDir, tar.TypeReg:
		default:
			continue
		}

		target, err := safeJoin(dst, h.Name)
		if err != nil {
			return err
		}

		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		if h.Size < 0 || h.Size > maxEntryBytes {
			return fmt.Errorf("tar entry too large: %d bytes", h.Size)
		}
		if extracted+h.Size > maxExtractBytes {
			return fmt.Errorf("tar total size exceeds limit")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		_, cpErr := io.CopyN(out, tr, h.Size)
		clErr := out.Close()
		if cpErr != nil && cpErr != io.EOF {
			return cpErr
		}
		if clErr != nil {
			return clErr
		}
		extracted += h.Size
	}
}
