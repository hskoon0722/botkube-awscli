package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MakeNowJust/heredoc"
	"github.com/google/shlex"
	"github.com/hashicorp/go-plugin"
	"github.com/kubeshop/botkube/pkg/api"
	"github.com/kubeshop/botkube/pkg/api/executor"
	"gopkg.in/yaml.v3"
)

const (
	pluginName = "aws"

	// 디컴프 방어 한도
	maxEntryBytes   = int64(128 << 20) // 128MiB per entry
	maxExtractBytes = int64(512 << 20) // 512MiB total
)

// 릴리스 번들 URL (env 로 오버라이드 가능)
// AWSCLI_TARBALL_URL_AMD64 / AWSCLI_TARBALL_URL_ARM64
var defaultBundleURL = map[string]string{
	"amd64": "https://github.com/hskoon0722/botkube-awscli/releases/download/v0.0.0-rc.3/aws_linux_amd64.tar.gz",
	"arm64": "",
}

type Config struct {
	DefaultRegion string            `yaml:"defaultRegion,omitempty"`
	PrependArgs   []string          `yaml:"prependArgs,omitempty"`
	Allowed       []string          `yaml:"allowed,omitempty"`
	Env           map[string]string `yaml:"env,omitempty"`
}

type Executor struct{}

func main() {
	executor.Serve(map[string]plugin.Plugin{
		pluginName: &executor.Plugin{Executor: &Executor{}},
	})
}

// ---------- 공통 유틸 ----------

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

// base 밖으로 못 나가게 안전 조인
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

// ---------- 번들(tar.gz: awscli/dist + glibc/*) 우선 ----------

func ensureFromBundle(ctx context.Context) (awsBin, glibcDir, distDir string, _ error) {
	dd, err := depsDir()
	if err != nil {
		return "", "", "", err
	}
	bundleRoot := filepath.Join(dd, "bundle")
	distDir = filepath.Join(bundleRoot, "awscli", "dist")
	glibcDir = filepath.Join(bundleRoot, "glibc")
	awsBin = filepath.Join(distDir, "aws")

	// 이미 준비됨?
	if st, err := os.Stat(awsBin); err == nil && (st.Mode().Perm()&0o111) != 0 {
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
		return "", "", "", fmt.Errorf("no bundle url configured for arch %q", arch)
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

// tar.gz 안전 추출 (경로/사이즈 검증)
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

// ---------- (백업) AWS 공식 zip에서 dist만 추출 ----------

func ensureFromOfficialZip(ctx context.Context) (awsBin, glibcDir, distDir string, _ error) {
	dd, err := depsDir()
	if err != nil {
		return "", "", "", err
	}
	distDir = filepath.Join(dd, "aws-official", "dist")
	awsBin = filepath.Join(distDir, "aws")

	if st, err := os.Stat(awsBin); err == nil && (st.Mode().Perm()&0o111) != 0 {
		return awsBin, "", distDir, nil
	}

	if err := os.MkdirAll(distDir, 0o755); err != nil {
		return "", "", "", err
	}

	var url string
	switch runtime.GOARCH {
	case "amd64":
		url = "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip"
	case "arm64":
		url = "https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip"
	default:
		return "", "", "", fmt.Errorf("unsupported arch: %s", runtime.GOARCH)
	}

	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("awscliv2-%d.zip", time.Now().UnixNano()))
	if err := httpGetToFile(ctx, url, tmp); err != nil {
		return "", "", "", fmt.Errorf("download aws zip: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()

	r, err := zip.OpenReader(tmp)
	if err != nil {
		return "", "", "", err
	}
	defer r.Close()

	const prefix = "aws/dist/"
	var extracted int64

	for _, f := range r.File {
		name := filepath.ToSlash(f.Name)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(name, prefix)

		if f.Mode()&os.ModeSymlink != 0 {
			continue
		}

		if f.UncompressedSize64 > math.MaxInt64 {
			return "", "", "", fmt.Errorf("zip entry too large for this platform: %d", f.UncompressedSize64)
		}
		want := int64(f.UncompressedSize64)
		if want > maxEntryBytes {
			return "", "", "", fmt.Errorf("zip entry too large: %d bytes", want)
		}
		if extracted+want > maxExtractBytes {
			return "", "", "", fmt.Errorf("zip total size exceeds limit")
		}

		dstPath, err := safeJoin(distDir, rel)
		if err != nil {
			return "", "", "", err
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dstPath, 0o755); err != nil {
				return "", "", "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return "", "", "", err
		}

		rc, err := f.Open()
		if err != nil {
			return "", "", "", err
		}
		out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			_ = rc.Close()
			return "", "", "", err
		}

		_, cpErr := io.CopyN(out, rc, want)
		rcCloseErr := rc.Close()
		outCloseErr := out.Close()
		if cpErr != nil && cpErr != io.EOF {
			return "", "", "", cpErr
		}
		if rcCloseErr != nil {
			return "", "", "", rcCloseErr
		}
		if outCloseErr != nil {
			return "", "", "", outCloseErr
		}
		extracted += want
	}

	_ = os.Chmod(awsBin, 0o755)
	return awsBin, "", distDir, nil
}

// ---------- Botkube 인터페이스 ----------

func (e *Executor) Metadata(context.Context) (api.MetadataOutput, error) {
	return api.MetadataOutput{
		Version:     "0.1.1",
		Description: "Run AWS CLI from chat.",
		JSONSchema: api.JSONSchema{
			Value: heredoc.Doc(`{
			  "$schema":"http://json-schema.org/draft-04/schema#",
			  "title":"aws",
			  "type":"object",
			  "properties":{
			    "defaultRegion":{"type":"string"},
			    "prependArgs":{"type":"array","items":{"type":"string"}},
			    "allowed":{"type":"array","items":{"type":"string"}},
			    "env":{"type":"object","additionalProperties":{"type":"string"}}
			  },
			  "additionalProperties": false
			}`),
		},
	}, nil
}

func (e *Executor) Help(context.Context) (api.Message, error) {
	btn := api.NewMessageButtonBuilder()
	return api.Message{
		Sections: []api.Section{{
			Base: api.Base{
				Header:      "Run AWS CLI",
				Description: "예) `aws --version`, `aws sts get-caller-identity`, `aws ec2 describe-instances --max-items 5`",
			},
			Buttons: []api.Button{
				btn.ForCommandWithDescCmd("Who am I?", "aws sts get-caller-identity"),
				btn.ForCommandWithDescCmd("Version", "aws --version"),
			},
		}},
	}, nil
}

func (e *Executor) Execute(ctx context.Context, in executor.ExecuteInput) (executor.ExecuteOutput, error) { //nolint:gocritic
	var cfg Config
	if err := mergeExecutorConfigs(in.Configs, &cfg); err != nil {
		return msg(err.Error()), nil
	}

	cmdLine := strings.TrimSpace(in.Command)
	if cmdLine == "" {
		return msg("Empty command"), nil
	}
	if strings.HasPrefix(cmdLine, pluginName) {
		cmdLine = strings.TrimSpace(strings.TrimPrefix(cmdLine, pluginName))
	}
	if len(cfg.Allowed) > 0 && !isAllowed(cmdLine, cfg.Allowed) {
		return msg(fmt.Sprintf("Command not allowed: %q", cmdLine)), nil
	}
	if len(cfg.PrependArgs) > 0 {
		cmdLine = strings.Join(append(append([]string{}, cfg.PrependArgs...), cmdLine), " ")
	}

	awsBin, glibcDir, distDir, err := ensureFromBundle(ctx)
	useLoader := err == nil && glibcDir != ""
	if err != nil {
		awsBin, glibcDir, distDir, err = ensureFromOfficialZip(ctx)
		if err != nil {
			return msg("failed to prepare aws cli: " + err.Error()), nil
		}
	}

	args, err := shlex.Split(cmdLine)
	if err != nil {
		return msg("invalid arguments: " + err.Error()), nil
	}

	// 로더 탐색 (기본 + 글롭 백업)
	ld := ""
	if useLoader {
		ld = findLoader(glibcDir)
		if ld == "" && glibcDir != "" {
			if cands, _ := filepath.Glob(filepath.Join(glibcDir, "ld-linux-*.so.*")); len(cands) > 0 {
				ld = cands[0]
				_ = os.Chmod(ld, 0o755)
			}
		}
	}

	// env 구성 (LD_LIBRARY_PATH = glibcDir:distDir)
	env := os.Environ()
	env = append(env, "HOME=/tmp", "AWS_PAGER=")
	if cfg.DefaultRegion != "" {
		env = append(env, "AWS_DEFAULT_REGION="+cfg.DefaultRegion)
	}
	if glibcDir != "" || distDir != "" {
		lp := ""
		if glibcDir != "" {
			lp = glibcDir
		}
		if distDir != "" {
			if lp != "" {
				lp += ":"
			}
			lp += distDir
		}
		env = append(env, "LD_LIBRARY_PATH="+lp)
	}
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}

	var cmd *exec.Cmd
	if useLoader && ld != "" {
		// 로더 경유 실행 (가장 확실)
		libraryPath := glibcDir
		if distDir != "" {
			if libraryPath != "" {
				libraryPath += ":"
			}
			libraryPath += distDir
		}
		loaderArgs := append([]string{"--library-path", libraryPath, awsBin}, args...)
		cmd = exec.CommandContext(ctx, ld, loaderArgs...)
	} else {
		// 로더가 없으면 LD_LIBRARY_PATH만으로 시도
		cmd = exec.CommandContext(ctx, awsBin, args...)
	}
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if err != nil {
		dbg := fmt.Sprintf("DBG useLoader=%t ld=%q aws=%q glibcDir=%q distDir=%q",
			useLoader, ld, awsBin, glibcDir, distDir)
		if outStr == "" {
			return msg(dbg + "\nERROR: " + err.Error()), nil
		}
		return msg(dbg + "\n" + outStr + "\nERROR: " + err.Error()), nil
	}
	if outStr == "" {
		outStr = "(no output)"
	}
	return executor.ExecuteOutput{Message: api.NewCodeBlockMessage(outStr, true)}, nil
}

func findLoader(glibcDir string) string {
	candidates := []string{
		filepath.Join(glibcDir, "ld-linux-x86-64.so.2"),
		filepath.Join(glibcDir, "ld-linux-aarch64.so.1"),
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && (st.Mode().Perm()&0o111) != 0 {
			return p
		}
	}
	return ""
}

func mergeExecutorConfigs(configs []*executor.Config, out *Config) error {
	if out.Env == nil {
		out.Env = map[string]string{}
	}
	for _, c := range configs {
		if c == nil || len(c.RawYAML) == 0 {
			continue
		}
		var t Config
		if err := yaml.Unmarshal(c.RawYAML, &t); err != nil {
			return err
		}
		if t.DefaultRegion != "" {
			out.DefaultRegion = t.DefaultRegion
		}
		if len(t.PrependArgs) > 0 {
			out.PrependArgs = t.PrependArgs
		}
		if len(t.Allowed) > 0 {
			out.Allowed = t.Allowed
		}
		for k, v := range t.Env {
			out.Env[k] = v
		}
	}
	return nil
}

func isAllowed(cmd string, allow []string) bool {
	cmd = strings.TrimSpace(cmd)
	for _, p := range allow {
		if strings.HasPrefix(cmd, strings.TrimSpace(p)) {
			return true
		}
	}
	return false
}

func msg(s string) executor.ExecuteOutput {
	return executor.ExecuteOutput{Message: api.NewPlaintextMessage(s, true)}
}
