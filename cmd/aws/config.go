package main

import (
	"strings"

	"github.com/kubeshop/botkube/pkg/api/executor"
	"gopkg.in/yaml.v3"
)

// Config holds executor configuration.
type Config struct {
	DefaultRegion string            `yaml:"defaultRegion,omitempty"`
	PrependArgs   []string          `yaml:"prependArgs,omitempty"`
	Allowed       []string          `yaml:"allowed,omitempty"`
	Env           map[string]string `yaml:"env,omitempty"`
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
