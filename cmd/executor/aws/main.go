package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/hashicorp/go-plugin"
	"github.com/kubeshop/botkube/pkg/api"
	"github.com/kubeshop/botkube/pkg/api/executor"
	bkplugin "github.com/kubeshop/botkube/pkg/plugin"
	"gopkg.in/yaml.v3"
)

const (
	pluginName = "aws"
	awsDepName = "aws"
)

type Config struct {
	DefaultRegion string            `yaml:"defaultRegion,omitempty"`
	PrependArgs   []string          `yaml:"prependArgs,omitempty"`
	Allowed       []string          `yaml:"allowed,omitempty"` // "s3 ls", "ec2 describe-instances" 같은 화이트리스트 (prefix 매칭)
	Env           map[string]string `yaml:"env,omitempty"`     // 추가 환경변수
}

type Executor struct{}

func main() {
	executor.Serve(map[string]plugin.Plugin{
		pluginName: &executor.Plugin{Executor: &Executor{}},
	})
}

// --- 필수: 메타데이터 + 의존성(AWS CLI 바이너리)
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
		Dependencies: map[string]api.Dependency{
			// zip 안의 단일 실행파일만 추출: '<zipURL>//<내부경로>?archive=zip'
			awsDepName: {
				URLs: map[string]string{
					"linux/amd64": "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip//aws/dist/aws?archive=zip",
					"linux/arm64": "https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip//aws/dist/aws?archive=zip",
				},
			},
		},
	}, nil
}

// --- 필수: 도움말
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

// --- 필수: 실행 로직
func (e *Executor) Execute(ctx context.Context, in executor.ExecuteInput) (executor.ExecuteOutput, error) { //nolint:gocritic // interface
	// 구성 병합 (바인딩된 여러 config를 순서대로 합침)
	var cfg Config
	if err := mergeExecutorConfigs(in.Configs, &cfg); err != nil {
		return executor.ExecuteOutput{}, err
	}

	// in.Command 에는 사용자가 친 원문 커맨드가 옵니다. (예: "aws ec2 describe-instances")
	cmd := strings.TrimSpace(in.Command)
	if cmd == "" {
		return msg("Empty command"), nil
	}
	// plugin 이름 앞부분 제거
	if strings.HasPrefix(cmd, pluginName) {
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, pluginName))
	}

	// 화이트리스트 검사 (선택)
	if len(cfg.Allowed) > 0 && !isAllowed(cmd, cfg.Allowed) {
		return msg(fmt.Sprintf("Command not allowed: %q", cmd)), nil
	}

	// Prepend args (선택)
	if len(cfg.PrependArgs) > 0 {
		cmd = strings.Join(append([]string{}, append(cfg.PrependArgs, cmd)...), " ")
	}

	// 최종 실행 문자열: 'aws <args...>'
	run := strings.TrimSpace("aws " + cmd)

	// 환경변수 구성
	env := map[string]string{
		"HOME":      "/tmp", // AWS CLI가 캐시 디렉터리를 쓸 수 있게
		"AWS_PAGER": "",     // less 방지
	}
	if cfg.DefaultRegion != "" {
		env["AWS_DEFAULT_REGION"] = cfg.DefaultRegion
	}
	for k, v := range cfg.Env {
		env[k] = v
	}

	// 의존성 치환 및 실행 (aws -> 실제 경로)
	out, err := bkplugin.ExecuteCommand(ctx, run, bkplugin.ExecuteCommandEnvs(env))
	stdout := strings.TrimSpace(string(out.Stdout))
	stderr := strings.TrimSpace(string(out.Stderr))

	if err != nil || out.ExitCode != 0 {
		msg := stdout
		if stderr != "" {
			if msg != "" {
				msg += "\n"
			}
			msg += "STDERR:\n" + stderr
		}
		if err != nil {
			if msg != "" {
				msg += "\n"
			}
			msg += "ERROR: " + err.Error()
		}
		return executor.ExecuteOutput{
			Message: api.NewPlaintextMessage(msg, true),
		}, nil
	}
	return executor.ExecuteOutput{
		Message: api.NewCodeBlockMessage(stdout, true),
	}, nil
}

func mergeExecutorConfigs(configs []*executor.Config, out *Config) error {
	// 기본값 초기화
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
