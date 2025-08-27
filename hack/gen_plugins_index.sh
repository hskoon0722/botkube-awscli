#!/usr/bin/env bash
set -euo pipefail

# Inputs
BASE="${PLUGIN_DOWNLOAD_URL_BASE_PATH:-}"
NAME="${NAME:-aws}"
TYPE="${TYPE:-executor}"
DESC="${DESC:-Run AWS CLI from chat.}"

# Derive tag and version
TAG="${GITHUB_REF#refs/tags/}"
if [[ -z "${TAG}" || "${TAG}" == "refs/tags/" ]]; then
  TAG="${TAG:-$(git describe --tags --exact-match 2>/dev/null || true)}"
fi
VERSION="${VERSION:-${TAG#v}}"

if [[ -z "${BASE}" ]]; then
  if [[ -n "${GITHUB_REPOSITORY:-}" && -n "${TAG}" ]]; then
    BASE="https://github.com/${GITHUB_REPOSITORY}/releases/download/${TAG}"
  else
    # best-effort local default
    repo_url="$(git config --get remote.origin.url | sed -E 's#^git@github.com:#https://github.com/#; s#\.git$##')"
    BASE="${repo_url}/releases/download/${TAG}"
  fi
fi

: > plugins-index.yaml
{
  echo "entries:";
  echo "  - name: ${NAME}";
  echo "    type: ${TYPE}";
  echo "    description: ${DESC}";
  echo "    version: ${VERSION}";
  echo "    urls:";

  # binaries
  if [[ -f dist/executor_aws_linux_amd64 ]]; then
    echo "      - url: ${BASE}/executor_aws_linux_amd64";
    echo "        platform:";
    echo "          os: linux";
    echo "          architecture: amd64";
  fi
  if [[ -f dist/executor_aws_linux_arm64 ]]; then
    echo "      - url: ${BASE}/executor_aws_linux_arm64";
    echo "        platform:";
    echo "          os: linux";
    echo "          architecture: arm64";
  fi

  # json schema inline; keep in sync with cmd/aws/main.go Metadata
  cat <<'JSON'
    jsonSchema:
      value: |-
          {
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
          }
JSON

  echo "    recommended: false";

  # dependencies (optional)
  echo "    dependencies:";
  echo "      - name: aws";
  echo "        urls:";
  if [[ -f dist/aws_linux_amd64.tar.gz ]]; then
    echo "          - url: ${BASE}/aws_linux_amd64.tar.gz//awscli/dist/aws?archive=tar.gz";
    echo "            platform:";
    echo "              os: linux";
    echo "              architecture: amd64";
  fi
  if [[ -f dist/aws_linux_arm64.tar.gz ]]; then
    echo "          - url: ${BASE}/aws_linux_arm64.tar.gz//awscli/dist/aws?archive=tar.gz";
    echo "            platform:";
    echo "              os: linux";
    echo "              architecture: arm64";
  fi
} >> plugins-index.yaml

# Validate non-empty urls
if ! grep -q '^\s\+- url:' plugins-index.yaml; then
  echo "ERROR: No binary URLs added to plugins-index.yaml (dist/ missing executors?)" >&2
  if [[ "${GITHUB_ACTIONS:-false}" == "true" ]]; then
    exit 1
  fi
fi

echo "Generated plugins-index.yaml" >&2
