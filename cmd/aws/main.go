package main

import (
	"archive/tar"
	"compress/gzip"
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

	"github.com/google/shlex"
	"github.com/hashicorp/go-plugin"
	"github.com/kubeshop/botkube/pkg/api"
	"github.com/kubeshop/botkube/pkg/api/executor"
	"gopkg.in/yaml.v3"
)

const (
	pluginName = "aws"

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

// Config holds executor configuration.
type Config struct {
	DefaultRegion string            `yaml:"defaultRegion,omitempty"`
	PrependArgs   []string          `yaml:"prependArgs,omitempty"`
	Allowed       []string          `yaml:"allowed,omitempty"`
	Env           map[string]string `yaml:"env,omitempty"`
}

// Executor implements the Botkube executor plugin for AWS CLI.
type Executor struct{}

func main() {
	executor.Serve(map[string]plugin.Plugin{
		pluginName: &executor.Plugin{Executor: &Executor{}},
	})
}

// ---------- Utilities ----------

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

// ---------- Bundle-first (tar.gz: awscli/dist + glibc/*) ----------

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

// untarGzSafe extracts tar.gz safely with size/path checks
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

// ZIP fallback removed — only prebuilt bundles are supported.

// ---------- Botkube Interface ----------

// Metadata returns plugin metadata and schema.
func (e *Executor) Metadata(context.Context) (api.MetadataOutput, error) {
	const jsonSchema = `{
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
    }`
	return api.MetadataOutput{
		Version:     "0.1.1",
		Description: "Run AWS CLI from chat.",
		JSONSchema: api.JSONSchema{
			Value: jsonSchema,
		},
	}, nil
}

// Help returns interactive help message with common examples.
func (e *Executor) Help(context.Context) (api.Message, error) {
	btn := api.NewMessageButtonBuilder()

	identity := []api.Button{
		btn.ForCommandWithDescCmd("Who am I?", "aws sts get-caller-identity"),
		btn.ForCommandWithDescCmd("Version", "aws --version"),
	}
	compute := []api.Button{
		btn.ForCommandWithDescCmd("EC2 instances", "aws ec2 describe-instances"),
		btn.ForCommandWithDescCmd("EKS clusters", "aws eks list-clusters"),
		btn.ForCommandWithDescCmd("ECS clusters", "aws ecs list-clusters"),
		btn.ForCommandWithDescCmd("Lambda functions", "aws lambda list-functions"),
	}
	storage := []api.Button{
		btn.ForCommandWithDescCmd("S3 buckets", "aws s3api list-buckets"),
	}
	database := []api.Button{
		btn.ForCommandWithDescCmd("RDS instances", "aws rds describe-db-instances"),
		btn.ForCommandWithDescCmd("DynamoDB tables", "aws dynamodb list-tables"),
		btn.ForCommandWithDescCmd("ElastiCache clusters", "aws elasticache describe-cache-clusters"),
	}
	network := []api.Button{
		btn.ForCommandWithDescCmd("VPCs", "aws ec2 describe-vpcs"),
		btn.ForCommandWithDescCmd("Subnets", "aws ec2 describe-subnets"),
	}
	updates := []api.Button{
		btn.ForCommandWithDescCmd("EC2 RebootInstances (picker)", "aws ec2 reboot-instances --instance-ids <i-xxxxxxxxxxxxxxxxx>"),
	}

	return api.Message{
		OnlyVisibleForYou: true,
		Sections: []api.Section{
			{
				Base: api.Base{
					Header:      "Run AWS CLI",
					Description: "Examples: `aws --version`, `aws sts get-caller-identity`, `aws ec2 describe-instances --max-results 5`",
				},
				Buttons: identity,
			},
			{Base: api.Base{Header: "Compute (examples)"}, Buttons: compute},
			{Base: api.Base{Header: "Storage (examples)"}, Buttons: storage},
			{Base: api.Base{Header: "Database (examples)"}, Buttons: database},
			{Base: api.Base{Header: "Networking (examples)"}, Buttons: network},
			{
				Base: api.Base{
					Header:      "Limited Update operations",
					Description: "Operations may be restricted by policy.",
				},
				Buttons: updates,
			},
		},
	}, nil
}

// Execute runs an AWS CLI command according to provided input.
func (e *Executor) Execute(ctx context.Context, in executor.ExecuteInput) (executor.ExecuteOutput, error) { //nolint:gocritic
	var cfg Config
	if err := mergeExecutorConfigs(in.Configs, &cfg); err != nil {
		return msg(err.Error()), nil
	}

	raw := strings.TrimSpace(in.Command)
	lower := strings.ToLower(raw)
	// Help routing
	if lower == "" || lower == pluginName || lower == pluginName+" help" || lower == "help" {
		h, _ := e.Help(ctx)
		return executor.ExecuteOutput{Message: h}, nil
	}
	if lower == pluginName+" help full" || lower == "help full" || lower == pluginName+" help examples" || lower == "help examples" {
		return executor.ExecuteOutput{Message: api.NewCodeBlockMessage(fullHelpText(), true)}, nil
	}

	// Normalize command, check allowed list, apply prepend args
	cmdLine := normalizeCmd(raw)
	if len(cfg.Allowed) > 0 && !isAllowed(cmdLine, cfg.Allowed) {
		return msg(fmt.Sprintf("Command not allowed: %q", cmdLine)), nil
	}
	if len(cfg.PrependArgs) > 0 {
		cmdLine = strings.Join(append(append([]string{}, cfg.PrependArgs...), cmdLine), " ")
	}
	// Special helper commands
	if strings.HasPrefix(cmdLine, "helper reboot-ec2") {
		// Prepare AWS binary to query instance IDs
		awsBin, glibcDir, distDir, err := prepareAws(ctx)
		if err != nil {
			return msg("failed to prepare aws cli: " + err.Error()), nil
		}
		ld := resolveLoaderPath(glibcDir)
		libraryPath := buildLDPath(glibcDir, distDir)
		env := buildEnv(cfg, libraryPath)

		ids, qerr := listEC2InstanceIDs(ctx, ld, awsBin, libraryPath, env)
		if qerr != nil {
			return msg("failed to list instances: " + qerr.Error()), nil
		}
		if len(ids) == 0 {
			return msg("no instances found"), nil
		}
		builder := api.NewMessageButtonBuilder()
		buttons := make([]api.Button, 0, len(ids))
		for i, id := range ids {
			if i >= 30 {
				break
			}
			buttons = append(buttons, builder.ForCommandWithDescCmd(
				"Reboot "+id, "aws ec2 reboot-instances --instance-ids "+id,
			))
		}
		return executor.ExecuteOutput{Message: api.Message{
			Sections: []api.Section{{
				Base:    api.Base{Header: "Select instance to reboot"},
				Buttons: buttons,
			}},
		}}, nil
	}

	args, err := shlex.Split(cmdLine)
	if err != nil {
		return msg("invalid arguments: " + err.Error()), nil
	}

	// Prepare AWS binary/loader
	awsBin, glibcDir, distDir, err := prepareAws(ctx)
	if err != nil {
		return msg("failed to prepare aws cli: " + err.Error()), nil
	}
	ld := resolveLoaderPath(glibcDir)
	libraryPath := buildLDPath(glibcDir, distDir)
	env := buildEnv(cfg, libraryPath)

	// Execute
	out, runErr := runAWS(ctx, ld, awsBin, libraryPath, args, env)
	outStr := strings.TrimSpace(string(out))
	if runErr != nil {
		dbg := fmt.Sprintf(
			"DBG useLoader=%t ld=%q aws=%q glibcDir=%q distDir=%q",
			ld != "", ld, awsBin, glibcDir, distDir,
		)
		if outStr == "" {
			return msg(dbg + "\nERROR: " + runErr.Error()), nil
		}
		return msg(dbg + "\n" + outStr + "\nERROR: " + runErr.Error()), nil
	}
	if outStr == "" {
		outStr = "(no output)"
	}
	return executor.ExecuteOutput{Message: api.NewCodeBlockMessage(outStr, true)}, nil
}

func normalizeCmd(raw string) string {
	cmd := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(cmd), pluginName) {
		return strings.TrimSpace(cmd[len(pluginName):])
	}
	return cmd
}

func prepareAws(ctx context.Context) (awsBin, glibcDir, distDir string, _ error) {
	return ensureFromBundle(ctx)
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

// fullHelpText returns a long, example-rich help as a code block.
func fullHelpText() string {
	return strings.TrimSpace(`Run AWS CLI
ex) aws --version, aws sts get-caller-identity, aws ec2 describe-instances --max-results 5

@black aws sts get-caller-identity
@black aws --version

Compute
@black aws ec2 describe-instances
@black aws eks list-clusters
@black aws ecs list-clusters
@black aws lambda list-functions

Storage
@black aws s3api list-buckets

Database
@black aws rds describe-db-instances
@black aws dynamodb list-tables
@black aws elasticache describe-cache-clusters

Networking
@black aws ec2 describe-vpcs
@black aws ec2 describe-subnets

Limited Update operations
@black aws ec2 reboot-instances --instance-ids <i-xxxxxxxxxxxxxxxxx>`)
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

// isExecutable reports whether a file exists and has any execute bit set.
func isExecutable(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return (st.Mode().Perm() & 0o111) != 0
}
