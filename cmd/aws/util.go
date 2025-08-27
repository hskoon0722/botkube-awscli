package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kubeshop/botkube/pkg/api"
	"github.com/kubeshop/botkube/pkg/api/executor"
)

const (
	// Extraction limits for safety
	maxEntryBytes   = int64(128 << 20) // 128MiB per entry
	maxExtractBytes = int64(512 << 20) // 512MiB total
)

// Release bundle URLs (can be overridden via env)
// AWSCLI_TARBALL_URL_AMD64 / AWSCLI_TARBALL_URL_ARM64
var defaultBundleURL = map[string]string{
	"amd64": "https://github.com/hskoon0722/botkube-awscli/releases/download/v0.0.0-rc.3/aws_linux_amd64.tar.gz",
	"arm64": "",
}

func depsDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return exe + "_deps", nil
}

func httpGetToFile(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, cpErr := io.Copy(f, resp.Body)
	clErr := f.Close()
	if cpErr != nil {
		return cpErr
	}
	return clErr
}

// safeJoin prevents path traversal outside base
func safeJoin(base, name string) (string, error) {
	path := filepath.Join(base, name)
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if pathAbs != baseAbs && !strings.HasPrefix(pathAbs, baseAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe path: %s", name)
	}
	return pathAbs, nil
}

func msg(s string) executor.ExecuteOutput {
	return executor.ExecuteOutput{Message: api.NewPlaintextMessage(s, true)}
}

// isExecutable reports whether a file exists and has any execute bit set.
func isExecutable(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return (st.Mode().Perm() & 0o111) != 0
}

func normalizeCmd(raw string) string {
	cmd := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(cmd), pluginName) {
		return strings.TrimSpace(cmd[len(pluginName):])
	}
	return cmd
}

func resolveLoaderPath(glibcDir string) string {
	if glibcDir == "" {
		return ""
	}
	candidates := []string{
		filepath.Join(glibcDir, "ld-linux-x86-64.so.2"),
		filepath.Join(glibcDir, "ld-linux-aarch64.so.1"),
	}
	for _, p := range candidates {
		if isExecutable(p) {
			return p
		}
	}
	// Wildcard fallback
	if cands, _ := filepath.Glob(filepath.Join(glibcDir, "ld-linux-*.so.*")); len(cands) > 0 {
		_ = os.Chmod(cands[0], 0o755)
		return cands[0]
	}
	return ""
}

func buildLDPath(glibcDir, distDir string) string {
	switch {
	case glibcDir != "" && distDir != "":
		return glibcDir + ":" + distDir
	case glibcDir != "":
		return glibcDir
	case distDir != "":
		return distDir
	default:
		return ""
	}
}

func buildEnv(cfg Config, ldPath string) []string {
	env := os.Environ()
	env = append(env, "HOME=/tmp", "AWS_PAGER=")
	if cfg.DefaultRegion != "" {
		env = append(env, "AWS_DEFAULT_REGION="+cfg.DefaultRegion)
	}
	if ldPath != "" {
		env = append(env, "LD_LIBRARY_PATH="+ldPath)
	}
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func runAWS(ctx context.Context, ld, awsBin, libraryPath string, args, env []string) ([]byte, error) {
	var cmd *exec.Cmd
	if ld != "" {
		loaderArgs := append([]string{"--library-path", libraryPath, awsBin}, args...)
		cmd = exec.CommandContext(ctx, ld, loaderArgs...)
	} else {
		cmd = exec.CommandContext(ctx, awsBin, args...)
	}
	cmd.Env = env
	return cmd.CombinedOutput()
}

// listEC2InstanceIDs returns a list of instance IDs using AWS CLI.
func listEC2InstanceIDs(ctx context.Context, ld, awsBin, libraryPath string, env []string) ([]string, error) {
	args := []string{
		"ec2", "describe-instances",
		"--query", "Reservations[].Instances[].InstanceId",
		"--output", "text",
	}
	out, err := runAWS(ctx, ld, awsBin, libraryPath, args, env)
	if err != nil {
		return nil, fmt.Errorf("describe-instances: %w; output: %s", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	unique := make(map[string]struct{}, len(fields))
	ids := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, ok := unique[f]; ok {
			continue
		}
		unique[f] = struct{}{}
		ids = append(ids, f)
	}
	return ids, nil
}
