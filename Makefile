############
# Building #
############

build-plugins: ## Builds all plugins for all defined platforms
	goreleaser build --clean --snapshot
.PHONY: build-plugins

build-plugins-single: ## Builds all plugins only for current GOOS and GOARCH.
	goreleaser build --clean --single-target --snapshot
.PHONY: build-plugins-single

##############
# Generating #
##############

gen-plugin-index: ## Generate plugins YAML index file.
	bash hack/gen_plugins_index.sh
.PHONY: gen-plugin-index

########################
# AWS bundle packaging #
########################

aws-bundle-amd64: ## Build AWS CLI runtime bundle for amd64 (dist/aws_linux_amd64.tar.gz)
	bash hack/build_aws_bundle.sh amd64
.PHONY: aws-bundle-amd64

aws-bundle-arm64: ## Build AWS CLI runtime bundle for arm64 (dist/aws_linux_arm64.tar.gz)
	bash hack/build_aws_bundle.sh arm64
.PHONY: aws-bundle-arm64

aws-bundle-all: ## Build AWS bundles for all supported arches
	$(MAKE) aws-bundle-amd64
	$(MAKE) aws-bundle-arm64
.PHONY: aws-bundle-all

######################
# Release publishing #
######################

release-gh: ## Create/update GitHub release and upload assets
	bash hack/release_gh.sh
.PHONY: release-gh

release-all: ## Build, bundle, index, and publish release
	$(MAKE) build-plugins
	$(MAKE) aws-bundle-all
	$(MAKE) gen-plugin-index
	$(MAKE) release-gh
.PHONY: release-all

###############
# Developing  #
###############

fix-lint-issues: ## Automatically fix lint issues
	go mod tidy
	go mod verify
	golangci-lint run --fix "./..."
.PHONY: fix-lint-issues

fmt: ## Run gofmt on repository
	gofmt -s -w ./cmd ./pkg 2>/dev/null || true
.PHONY: fmt

#############
# Others    #
#############

help: ## Show this help
	@egrep -h '\s##\s' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
.PHONY: help
