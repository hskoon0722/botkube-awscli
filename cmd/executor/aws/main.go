package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MakeNowJust/heredoc"
	"github.com/hashicorp/go-plugin"
	"github.com/kubeshop/botkube/pkg/api"
	"github.com/kubeshop/botkube/pkg/api/executor"
	bkplugin "github.com/kubeshop/botkube/pkg/plugin"
	"gopkg.in/yaml.v3"
)

const (
	pluginName         = "aws"
	maxUnzipFileBytes  = 200 * 1024 * 1024 // 200MB
	maxUnzipTotalBytes = 600 * 1024 * 1024 // 600MB
)

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

func depsDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return exe + "_deps", nil
}

// AWS CLI v2 설치/준비 후 ldLibraryPath 반환.
// Botkube가 항상 "<exe>_deps/<first-token>" 형태로 실행하므로
// 첫 토큰 "aws"가 가리키도록 "<deps>/aws"를 최종적으로 만들어준다(심링크 또는 복사).
func ensureAWS(ctx context.Context) (awsShimPath, ldLibraryPath string, _ error) {
	dd, err := depsDir()
	if err != nil {
		return "", "", err
	}
	distDir := filepath.Join(dd, "awscli", "dist")
	finalAws := filepath.Join(distDir, "aws")
	shimAws := filepath.Join(dd, "aws") // Botkube가 찾는 위치

	// 이미 준비되어 있으면 바로 반환
	if st, err := os.Stat(finalAws); err == nil && (st.Mode().Perm()&0o111) != 0 {
		_ = ensureShimOrCopy(finalAws, shimAws)
		return shimAws, distDir, nil
	}

	if err := os.MkdirAll(distDir, 0o755); err != nil {
		return "", "", err
	}

	var url string
	switch runtime.GOARCH {
	case "amd64":
		url = "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip"
	case "arm64":
		url = "https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip"
	default:
		return "", "", fmt.Errorf("unsupported arch: %s", runtime.GOARCH)
	}

	tmpZip := filepath.Join(os.TempDir(), fmt.Sprintf("awscliv2-%d.zip", time.Now().UnixNano()))
	if err := downloadFile(ctx, url, tmpZip); err != nil {
		return "", "", fmt.Errorf("download awscli: %w", err)
	}
	defer os.Remove(tmpZip)

	if err := unzipAwsDist(tmpZip, distDir); err != nil {
		return "", "", fmt.Errorf("extract aws dist: %w", err)
	}
	_ = os.Chmod(finalAws, 0o755)

	if err := ensureShimOrCopy(finalAws, shimAws); err != nil {
		return "", "", err
	}

	return shimAws, distDir, nil
}

func ensureShimOrCopy(src, dst string) error {
	// 이미 존재하면서 실행 가능하면 OK
	if st, err := os.Lstat(dst); err == nil {
		if st.Mode()&0o111 != 0 {
			// 심볼릭 링크면 대상이 맞는지도 한번 확인
			if st.Mode()&os.ModeSymlink != 0 {
				if tgt, err := os.Readlink(dst); err == nil && tgt == src {
					return nil
				}
			} else {
				return nil
			}
		}
		_ = os.Remove(dst)
	}
	// 심볼릭 링크 시도
	if err := os.Symlink(src, dst); err == nil {
		return nil
	}
	// 심볼릭 링크 안되면 복사
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		_ = in.Close()
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeOutErr := out.Close()
	closeInErr := in.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeOutErr != nil {
		return closeOutErr
	}
	return closeInErr
}

func downloadFile(ctx context.Context, url, dst string) error {
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
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func unzipAwsDist(zipPath, destDist string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	var total uint64
	const prefix = "aws/dist/"

	for _, f := range r.File {
		name := filepath.ToSlash(f.Name)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if f.UncompressedSize64 > maxUnzipFileBytes {
			return fmt.Errorf("file too large in zip: %s (%d bytes)", name, f.UncompressedSize64)
		}
		total += f.UncompressedSize64
		if total > maxUnzipTotalBytes {
			return fmt.Errorf("unzip total exceeds limit (%d bytes)", total)
		}

		rel := strings.TrimPrefix(name, prefix)
		dstPath, err := safeJoin(destDist, rel)
		if err != nil {
			return err
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dstPath, 0o755); err != nil {
				return err
			}
			continue
		}
		// 링크/장치 방지
		if f.Mode()&os.ModeSymlink != 0 || (f.Mode()&os.ModeDevice) != 0 {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			_ = rc.Close()
			return err
		}

		limited := io.LimitReader(rc, int64(maxUnzipFileBytes))
		_, copyErr := io.Copy(out, limited)
		closeOutErr := out.Close()
		closeRCErr := rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeOutErr != nil {
			return closeOutErr
		}
		if closeRCErr != nil {
			return closeRCErr
		}
	}
	return nil
}

func safeJoin(base, rel string) (string, error) {
	clean := filepath.Clean(filepath.Join(base, rel))
	if clean != base && !strings.HasPrefix(clean, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("illegal path: %s", clean)
	}
	return clean, nil
}

func (e *Executor) Metadata(context.Context) (api.MetadataOutput, error) {
	return api.MetadataOutput{
		Version:     "0.1.0",
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
		Sections: []api.Section{
			{
				Base: api.Base{
					Header:      "Run AWS CLI",
					Description: "예) `aws --version`, `aws sts get-caller-identity`, `aws ec2 describe-instances --max-items 5`",
				},
				Buttons: []api.Button{
					btn.ForCommandWithDescCmd("Who am I?", "aws sts get-caller-identity"),
					btn.ForCommandWithDescCmd("Version", "aws --version"),
				},
			},
		},
	}, nil
}

func (e *Executor) Execute(ctx context.Context, in executor.ExecuteInput) (executor.ExecuteOutput, error) { //nolint:gocritic
	var cfg Config
	if err := mergeExecutorConfigs(in.Configs, &cfg); err != nil {
		return executor.ExecuteOutput{}, err
	}

	cmd := strings.TrimSpace(in.Command)
	if cmd == "" {
		return msg("Empty command"), nil
	}
	if strings.HasPrefix(cmd, pluginName) {
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, pluginName))
	}
	if len(cfg.Allowed) > 0 && !isAllowed(cmd, cfg.Allowed) {
		return msg(fmt.Sprintf("Command not allowed: %q", cmd)), nil
	}
	if len(cfg.PrependArgs) > 0 {
		cmd = strings.Join(append(append([]string{}, cfg.PrependArgs...), cmd), " ")
	}

	// AWS 설치 + shim 생성(…/_deps/aws)
	_, ldPath, err := ensureAWS(ctx)
	if err != nil {
		return msg("ERROR preparing aws cli: " + err.Error()), nil
	}

	run := strings.TrimSpace("aws " + cmd) // 절대경로 금지! Botkube가 _deps/를 앞에 붙임.
	env := map[string]string{
		"HOME":            "/tmp",
		"AWS_PAGER":       "",
		"LD_LIBRARY_PATH": ldPath,
	}
	if cfg.DefaultRegion != "" {
		env["AWS_DEFAULT_REGION"] = cfg.DefaultRegion
	}
	for k, v := range cfg.Env {
		env[k] = v
	}

	out, err := bkplugin.ExecuteCommand(ctx, run, bkplugin.ExecuteCommandEnvs(env))
	stdout := strings.TrimSpace(string(out.Stdout))
	stderr := strings.TrimSpace(string(out.Stderr))
	if err != nil || out.ExitCode != 0 {
		msgStr := stdout
		if stderr != "" {
			if msgStr != "" {
				msgStr += "\n"
			}
			msgStr += "STDERR:\n" + stderr
		}
		if err != nil {
			if msgStr != "" {
				msgStr += "\n"
			}
			msgStr += "ERROR: " + err.Error()
		}
		return executor.ExecuteOutput{Message: api.NewPlaintextMessage(msgStr, true)}, nil
	}
	return executor.ExecuteOutput{Message: api.NewCodeBlockMessage(stdout, true)}, nil
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
		if len(t.Env) > 0 {
			if out.Env == nil {
				out.Env = map[string]string{}
			}
			for k, v := range t.Env {
				out.Env[k] = v
			}
		}
	}
	return nil
}

func isAllowed(cmd string, allow []string) bool {
	for _, p := range allow {
		if strings.HasPrefix(strings.TrimSpace(cmd), strings.TrimSpace(p)) {
			return true
		}
	}
	return false
}

func msg(s string) executor.ExecuteOutput {
	return executor.ExecuteOutput{
		Message: api.NewPlaintextMessage(s, true),
	}
}
