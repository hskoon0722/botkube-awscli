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
	var extracted uint64 // 전체 추출 누적 바이트(== uint64로 유지)

	for _, f := range r.File {
		name := filepath.ToSlash(f.Name)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(name, prefix)

		// 심볼릭 링크 무시
		if f.Mode()&os.ModeSymlink != 0 {
			continue
		}

		entrySize := f.UncompressedSize64
		// 항목/전체 크기 제한 (전부 uint64 비교)
		if entrySize > uint64(maxEntryBytes) {
			return "", "", "", fmt.Errorf("zip entry too large: %d bytes", entrySize)
		}
		if extracted+entrySize > uint64(maxExtractBytes) {
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

		// entrySize만큼 정확히 복사되었는지 검증:
		// LimitReader 에는 int64 한도(maxEntryBytes)만 전달하고,
		// 실제 복사된 바이트 수로 entrySize와 일치 여부를 판단.
		n, cpErr := io.Copy(out, io.LimitReader(rc, maxEntryBytes))
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
		if entrySize > uint64(math.MaxInt64) {
			return "", "", "", fmt.Errorf("zip entry too large for this platform: %d", entrySize)
		}
		if n != int64(entrySize) {
			return "", "", "", fmt.Errorf("zip entry size mismatch: copied=%d want=%d (%s)", n, entrySize, rel)
		}

		extracted += entrySize
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

	// 공통 예시(자주 쓰는 것)
	identity := []api.Button{
		btn.ForCommandWithDescCmd("Who am I?", "aws sts get-caller-identity"),
		btn.ForCommandWithDescCmd("Version", "aws --version"),
	}

	// Compute
	compute := []api.Button{
		btn.ForCommandWithDescCmd("EC2 instances (5)", "aws ec2 describe-instances --max-results 5"),
		btn.ForCommandWithDescCmd("ASGs (10)", "aws autoscaling describe-auto-scaling-groups --max-items 10"),
		btn.ForCommandWithDescCmd("EKS clusters", "aws eks list-clusters"),
		btn.ForCommandWithDescCmd("ECS clusters", "aws ecs list-clusters"),
		btn.ForCommandWithDescCmd("Lambda functions (10)", "aws lambda list-functions --max-items 10"),
	}

	// Storage
	storage := []api.Button{
		btn.ForCommandWithDescCmd("S3 buckets", "aws s3api list-buckets"),
		btn.ForCommandWithDescCmd("EBS volumes (10)", "aws ec2 describe-volumes --max-results 10"),
		btn.ForCommandWithDescCmd("EFS filesystems", "aws efs describe-file-systems"),
		btn.ForCommandWithDescCmd("FSx filesystems", "aws fsx describe-file-systems"),
		btn.ForCommandWithDescCmd("Backups (10)", "aws backup list-backup-plans --max-results 10"),
	}

	// Database
	database := []api.Button{
		btn.ForCommandWithDescCmd("RDS instances (20)", "aws rds describe-db-instances --max-records 20"),
		btn.ForCommandWithDescCmd("DynamoDB tables (20)", "aws dynamodb list-tables --max-items 20"),
		btn.ForCommandWithDescCmd("ElastiCache clusters", "aws elasticache describe-cache-clusters"),
		btn.ForCommandWithDescCmd("Redshift clusters", "aws redshift describe-clusters"),
	}

	// Networking
	network := []api.Button{
		btn.ForCommandWithDescCmd("VPCs", "aws ec2 describe-vpcs"),
		btn.ForCommandWithDescCmd("Subnets (50)", "aws ec2 describe-subnets --max-results 50"),
		btn.ForCommandWithDescCmd("Security groups (50)", "aws ec2 describe-security-groups --max-results 50"),
		btn.ForCommandWithDescCmd("Route tables (50)", "aws ec2 describe-route-tables --max-results 50"),
		btn.ForCommandWithDescCmd("VPC Endpoints (50)", "aws ec2 describe-vpc-endpoints --max-results 50"),
	}

	// Monitoring & Logging
	monitoring := []api.Button{
		btn.ForCommandWithDescCmd("CW metrics (namespaces)", "aws cloudwatch list-metrics --recently-active PT3H --max-items 50"),
		btn.ForCommandWithDescCmd("Log groups (50)", "aws logs describe-log-groups --limit 50"),
		btn.ForCommandWithDescCmd("X-Ray groups", "aws xray get-group --group-name default"), // 예시
	}

	// Security & Compliance
	security := []api.Button{
		btn.ForCommandWithDescCmd("IAM roles (100)", "aws iam list-roles --max-items 100"),
		btn.ForCommandWithDescCmd("Config recorders", "aws configservice describe-configuration-recorders"),
		btn.ForCommandWithDescCmd("CloudTrail trails", "aws cloudtrail list-trails"),
	}

	// ⚠️ 제한적 Update (정책에 맞춰 극히 보수적으로)
	// 클릭 즉시 실행되므로 파라미터가 필요한 것들은 안내 문구 포함
	updates := []api.Button{
		btn.ForCommandWithDescCmd("ASG StartInstanceRefresh (필요: ASG 이름)",
			"aws autoscaling start-instance-refresh --auto-scaling-group-name <asg-name>"),
		btn.ForCommandWithDescCmd("EKS UpdateNodegroupVersion (필요: cluster/nodegroup)",
			"aws eks update-nodegroup-version --cluster-name <cluster> --nodegroup-name <nodegroup> --version <k8s-version>"),
		btn.ForCommandWithDescCmd("EC2 RebootInstances (제한됨: black 태그)",
			"aws ec2 reboot-instances --instance-ids <i-xxxxxxxxxxxxxxxxx>"),
	}

	return api.Message{
		Sections: []api.Section{
			{
				Base: api.Base{
					Header:      "Run AWS CLI",
					Description: "예) `aws --version`, `aws sts get-caller-identity`, `aws ec2 describe-instances --max-results 5`",
				},
				Buttons: identity,
			},
			{
				Base:    api.Base{Header: "Compute (Describe/List/Get)"},
				Buttons: compute,
			},
			{
				Base:    api.Base{Header: "Storage (Describe/List/Get)"},
				Buttons: storage,
			},
			{
				Base:    api.Base{Header: "Database (Describe/List/Get)"},
				Buttons: database,
			},
			{
				Base:    api.Base{Header: "Networking (Describe/List/Get)"},
				Buttons: network,
			},
			{
				Base:    api.Base{Header: "Monitoring & Logging"},
				Buttons: monitoring,
			},
			{
				Base:    api.Base{Header: "Security & Compliance"},
				Buttons: security,
			},
			{
				Base: api.Base{
					Header:      "Limited Update operations",
					Description: "클릭 즉시 실행됩니다. 필요한 파라미터(이름/버전/인스턴스ID 등)를 **반드시** 채워 넣으세요. 정책으로 호출이 제한될 수 있습니다.",
				},
				Buttons: updates,
			},
		},
	}, nil
}

func (e *Executor) Execute(ctx context.Context, in executor.ExecuteInput) (executor.ExecuteOutput, error) { //nolint:gocritic
	var cfg Config
	if err := mergeExecutorConfigs(in.Configs, &cfg); err != nil {
		return msg(err.Error()), nil
	}

	raw := strings.TrimSpace(in.Command)
	lower := strings.ToLower(raw)

	// 1) help 라우팅: 빈 입력 / "aws" / "aws help" / "help" 는 Help() 출력
	if lower == "" || lower == pluginName || lower == pluginName+" help" || lower == "help" {
		h, _ := e.Help(ctx)
		return executor.ExecuteOutput{Message: h}, nil
	}

	cmdLine := raw
	if strings.HasPrefix(strings.ToLower(cmdLine), pluginName) {
		cmdLine = strings.TrimSpace(cmdLine[len(pluginName):])
	}

	// 2) 허용 패턴 검사 (help 라우팅 이후에 수행)
	if len(cfg.Allowed) > 0 && !isAllowed(cmdLine, cfg.Allowed) {
		return msg(fmt.Sprintf("Command not allowed: %q", cmdLine)), nil
	}

	// 3) prependArgs 적용
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
