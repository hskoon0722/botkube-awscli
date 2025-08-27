package main

import (
	"github.com/hashicorp/go-plugin"
	"github.com/kubeshop/botkube/pkg/api/executor"
)

const pluginName = "aws"

// Executor implements the Botkube executor plugin for AWS CLI.
type Executor struct{}

func main() {
	executor.Serve(map[string]plugin.Plugin{
		pluginName: &executor.Plugin{Executor: &Executor{}},
	})
}
