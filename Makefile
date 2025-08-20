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
	go run github.com/kubeshop/botkube/hack \
	  -binaries-path "./dist" \
	  -url-base-path "${PLUGIN_DOWNLOAD_URL_BASE_PATH}" \
	  > plugins-index.yaml
.PHONY: gen-plugin-index

###############
# Developing  #
###############

fix-lint-issues: ## Automatically fix lint issues
	go mod tidy
	go mod verify
	golangci-lint run --fix "./..."
.PHONY: fix-lint-issues

#############
# Others    #
#############

help: ## Show this help
	@egrep -h '\s##\s' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
.PHONY: help
