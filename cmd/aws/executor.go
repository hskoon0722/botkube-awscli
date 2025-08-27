package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/shlex"
	"github.com/kubeshop/botkube/pkg/api"
	"github.com/kubeshop/botkube/pkg/api/executor"
)

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
