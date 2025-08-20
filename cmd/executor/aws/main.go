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
	pluginName = "aws"

	// gosec(G110) 예방: 해제 파일/총 용량 상한
	maxUnzipFileBytes  = 200 * 1024 * 1024 // 200MB
	maxUnzipTotalBytes = 600 * 1024 * 1024 // 600MB
)

type Config struct {
	DefaultRegion string            `yaml:"defaultRegion,omitempty"`
	PrependArgs   []string          `yaml:"prependArgs,omitempty"`
	Allowed       []string          `yaml:"allowed,omitempty"` // "s3 ls", "ec2 describe-instances" 같은 prefix 화이트리스트
	Env           map[string]string `yaml:"env,omitempty"`     // 추가 환경변수
}

type Executor struct{}

func main() {
	executor.Serve(map[string]plugin.Plugin{
		pluginName: &executor.Plugin{Executor: &Executor{}},
	})
}

// 실행 파일 옆에 deps 디렉터리
func depsDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return exe + "_deps", nil
}

// AWS CLI v2가 없으면 내려받아 설치하고, 실행 경로와 LD_LIBRARY_PATH로 쓸 dist 경로를 돌려준다.
func ensureAWS(ctx context.Context) (awsPath string, ldLibraryPath string, _ error) {
	dd, err := depsDir()
	if err != nil {
		return "", "", err
	}

	distDir := filepath.Join(dd, "awscli", "dist")
	finalAws := filepath.Join(distDir, "aws")

	// 이미 설치됨
	if st, err := os.Stat(finalAws); err == nil && (st.Mode().Perm()&0o111) != 0 {
		return finalAws, distDir, nil
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

	// ZIP 안에서 "aws/dist/*" 만 distDir로 추출 (안전 checks 포함)
	if err := unzipAwsDist(tmpZip, distDir); err != nil {
		return "", "", fmt.Errorf("extract aws dist: %w", err)
	}

	_ = os.Chmod(finalAws, 0o755)
	return finalAws, distDir, nil
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
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
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
		// 각 파일 상한
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
		// (가능하면) 심볼릭 링크 배제
		if f.Mode()&os.ModeSymlink != 0 {
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		func() {
			defer rc.Close()
			out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return
			}
			defer out.Close()
			// 제한된 복사 (헤더 크기 + 파일별 상한 방어)
			lr := &io.LimitedReader{R: rc, N: int64(f.UncompressedSize64)}
			_, _ = io.Copy(out, lr)
		}()
	}
	return nil
}

func safeJoin(base, rel string) (string, error) {
	clean := filepath.Clean(filepath.Join(base, rel))
	// 경로 탈출 방지
	if clean != base && !strings.HasPrefix(clean, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("illegal path: %s", clean)
	}
	return clean, nil
}

// --- 메타데이터 (Dependencies 제거; 런타임 ensureAWS 사용)
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

// --- 도움말
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

// --- 실행 로직
func (e *Executor) Execute(ctx context.Context, in executor.ExecuteInput) (executor.ExecuteOutput, error) { //nolint:gocritic // interface
	// 구성 병합
	var cfg Config
	if err := mergeExecutorConfigs(in.Configs, &cfg); err != nil {
		return executor.ExecuteOutput{}, err
	}

	// 원문 커맨드
	cmd := strings.TrimSpace(in.Command)
	if cmd == "" {
		return msg("Empty command"), nil
	}
	// plugin 이름 접두어 제거
	if strings.HasPrefix(cmd, pluginName) {
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, pluginName))
	}

	// 화이트리스트 검사
	if len(cfg.Allowed) > 0 && !isAllowed(cmd, cfg.Allowed) {
		return msg(fmt.Sprintf("Command not allowed: %q", cmd)), nil
	}

	// prepend
	if len(cfg.PrependArgs) > 0 {
		cmd = strings.Join(append(append([]string{}, cfg.PrependArgs...), cmd), " ")
	}

	// AWS CLI 준비 (없으면 다운로드/설치)
	awsPath, ldPath, err := ensureAWS(ctx)
	if err != nil {
		return msg("ERROR preparing aws cli: " + err.Error()), nil
	}

	// 절대경로로 실행
	run := strings.TrimSpace(awsPath + " " + cmd)

	// env
	env := map[string]string{
		"HOME":             "/tmp", // 캐시/설정용
		"AWS_PAGER":        "",
		"LD_LIBRARY_PATH":  ldPath, // awscli v2의 dist
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
		return executor.ExecuteOutput{
			Message: api.NewPlaintextMessage(msgStr, true),
		}, nil
	}
	return executor.ExecuteOutput{
		Message: api.NewCodeBlockMessage(stdout, true),
	}, nil
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
