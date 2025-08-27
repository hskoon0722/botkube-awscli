#!/usr/bin/env bash
set -euo pipefail

# Inputs
TAG="${GITHUB_REF#refs/tags/}"
if [[ -z "${TAG}" || "${TAG}" == "refs/tags/" ]]; then
  # Fallback to exact tag if running outside Actions
  TAG="$(git describe --tags --exact-match 2>/dev/null || true)"
fi
if [[ -z "${TAG}" ]]; then
  echo "TAG not detected. Set GITHUB_REF or run on a tag." >&2
  exit 2
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "GitHub CLI 'gh' is required." >&2
  exit 2
fi

PLUGIN_DOWNLOAD_URL_BASE_PATH="${PLUGIN_DOWNLOAD_URL_BASE_PATH:-}"
if [[ -z "${PLUGIN_DOWNLOAD_URL_BASE_PATH}" ]]; then
  # Best effort default for local runs
  repo_url="$(git config --get remote.origin.url | sed -E 's#^git@github.com:#https://github.com/#; s#\.git$##')"
  repo_path="${repo_url#https://github.com/}"
  PLUGIN_DOWNLOAD_URL_BASE_PATH="https://github.com/${repo_path}/releases/download/${TAG}"
fi

# 1) Prepare release notes
cat > release.md <<EOF
Botkube Plugins ${TAG} are now available!
To use them:
```yaml
plugins:
  repositories:
    $(basename "$(pwd)"):
      url: ${PLUGIN_DOWNLOAD_URL_BASE_PATH}/plugins-index.yaml
```
EOF

# 2) Create or update release
if ! gh release view "${TAG}" >/dev/null 2>&1; then
  gh release create "${TAG}" --notes-file release.md
else
  gh release edit "${TAG}" --notes-file release.md
fi

# 3) Upload assets
shopt -s nullglob
assets=(dist/executor_* dist/aws_linux_*.tar.gz plugins-index.yaml)
echo "Uploading assets: ${assets[*]}"
if ((${#assets[@]})); then
  gh release upload "${TAG}" "${assets[@]}" --clobber
else
  echo "No assets to upload."
fi

echo "Index URL => ${PLUGIN_DOWNLOAD_URL_BASE_PATH}/plugins-index.yaml"

