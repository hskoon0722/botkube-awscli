package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
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

// depsDir returns "<plugin-binary>_deps".
func depsDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return exe + "_deps", nil
}

// ensureAWS downloads and extracts AWS CLI v2 "aws/dist" into deps dir if missing.
func ensureAWS(ctx context.Context) (awsPath, ldLibraryPath string, _ error) {
	dd, err := depsDir()
	if err != nil {
		return "", "", err
	}
	distDir := filepath.Join(dd, "awscli", "dist")
	finalAws := filepath.Join(distDir, "aws")

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
	defer func() { _ = os.Remove(tmpZip) }()

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
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, cpErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if cpErr != nil {
		return cpErr
	}
	return closeErr
}

// unzipAwsDist extracts only entries under "aws/dist/" to destDist.
//
//nolint:gosec // We extract known vendor zip (AWS CLI v2) and restrict to "aws/dist/*".
func unzipAwsDist(zipPath, destDist string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	const prefix = "aws/dist/"
	for _, f := range r.File {
		// normalize to forward slashes
		name := filepath.ToSlash(f.Name)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(name, prefix)
		dstPath := filepath.Join(destDist, rel)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dstPath, 0o755); err != nil {
				return err
			}
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

		_, cpErr := io.Copy(out, rc)
		rcCloseErr := rc.Close()
		outCloseErr := out.Close()
		if cpErr != nil {
			return cpErr
		}
		if rcCloseErr != nil {
			return rcCloseErr
		}
		if outCloseErr != nil {
			return outCloseErr
		}
	}
	return nil
}

// --- Metadata: NO Dependencies here to avoid Botkube path rewriting.
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

func (e *Executor) Execute(ctx context.Context, in executor.ExecuteInput) (executor.ExecuteOutput, error) { //nolint:gocritic // interface signature fixed by framework
	// merge configs
	var cfg Config
	if err := mergeExecutorConfigs(in.Configs, &cfg); err != nil {
		return msg(err.Error()), nil
	}

	// original user command (e.g. "aws ec2 describe-instances")
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

	awsBin, ldPath, err := ensureAWS(ctx)
	if err != nil {
		return msg("failed to prepare aws cli: " + err.Error()), nil
	}

	// Parse args safely (handles quotes).
	args, err := shlex.Split(cmdLine)
	if err != nil {
		return msg("invalid arguments: " + err.Error()), nil
	}

	// Build command: <awsBin> <args...>
	cmd := exec.CommandContext(ctx, awsBin, args...) // no Botkube wrapper => no dependency rewrite
	// env
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
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if err != nil {
		if outStr == "" {
			return msg("ERROR: " + err.Error()), nil
		}
		return msg(outStr+"\nERROR: "+err.Error()), nil
	}
	if outStr == "" {
		outStr = "(no output)"
	}
	return executor.ExecuteOutput{Message: api.NewCodeBlockMessage(outStr, true)}, nil
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
