# Botkube AWS CLI Executor

Run AWS CLI from chat via a Botkube executor plugin. Examples: `aws --version`, `aws sts get-caller-identity`, `aws ec2 describe-instances`.

The plugin uses a prebuilt AWS CLI bundle (awscli/dist + glibc) as a dependency and distributes itself via a plugin repository index (`plugins-index.yaml`).

By default, the executor downloads the latest bundle from GitHub Releases (no env required). To pin a specific version or use a mirror, set:
- `AWSCLI_TARBALL_URL_AMD64`
- `AWSCLI_TARBALL_URL_ARM64`


## Requirements

- [Go](https://golang.org/doc/install) >= 1.18
- [GoReleaser](https://goreleaser.com/) >= 1.13
- Docker (required for building the arm64 bundle)
- GitHub CLI `gh` (for uploading releases)

## Local Build

1) Build executor binaries
- `make build-plugins`
  - Produces `dist/executor_aws_linux_amd64` and `dist/executor_aws_linux_arm64`

2) Build AWS CLI bundles (amd64 and arm64)
- `make aws-bundle-all`
  - Produces `dist/aws_linux_amd64.tar.gz` and `dist/aws_linux_arm64.tar.gz`

3) Generate plugin index
- `export PLUGIN_DOWNLOAD_URL_BASE_PATH="https://github.com/<owner>/<repo>/releases/download/<tag>"`
- `make gen-plugin-index`
  - Creates `plugins-index.yaml` with URLs for available binaries/bundles

4) Formatting (optional)
- `make fmt` (runs gofmt)

## Release (Publish)

Create a GitHub Release with assets (binaries, bundles, plugins-index.yaml).

- Release notes template: `.github/release-notes.md`
  - Variables: `${TAG}`, `${REPO_NAME}`, `${PLUGIN_DOWNLOAD_URL_BASE_PATH}`
  - Override with `RELEASE_NOTES_FILE` if needed

Option A) Tag-triggered via GitHub Actions
- `git tag vX.Y.Z && git push origin vX.Y.Z`
- The workflow builds binaries → builds bundles → generates index → creates/updates the Release and uploads assets

Option B) Manual (for testing)
- `make build-plugins && make aws-bundle-all`
- `export PLUGIN_DOWNLOAD_URL_BASE_PATH=... && make gen-plugin-index`
- `export GH_TOKEN=<token> && make release-gh`

## Register in Botkube

Add the plugin repository to your Botkube config (values.yaml):

```yaml
plugins:
  repositories:
    awscli-repo:
      url: https://github.com/<owner>/<repo>/releases/download/vX.Y.Z/plugins-index.yaml
```

Then configure the executor and bindings so you can run `aws ...` commands from your chat platform.

## Make Targets

- `build-plugins`: Build executor binaries into `dist/` via GoReleaser snapshot.
- `build-plugins-single`: Build only for current GOOS/GOARCH.
- `aws-bundle-amd64` / `aws-bundle-arm64`: Build AWS CLI runtime bundles per arch.
- `aws-bundle-all`: Build both runtime bundles.
- `gen-plugin-index`: Generate `plugins-index.yaml` from `dist/` and base URL.
- `release-gh`: Create/update GitHub release and upload assets.
- `release-all`: Build → bundle → index → release.
- `fmt`: Run `gofmt` locally to format Go files.

Notes:
- `gen-plugin-index` uses `PLUGIN_DOWNLOAD_URL_BASE_PATH` to build absolute URLs. In CI this is set automatically; locally you must export it.
- The CI script fails if no binary URLs are found to prevent uploading an invalid index.

Slack setup note:
- If you see errors like `missing_scope` or `message_not_found` in logs, ensure your Slack app has required scopes and is reinstalled: `users:read`, `chat:write` (and possibly `chat:write.public`), `channels:read`, `channels:history`, `groups:read`, `groups:history`, `im:read`, `im:history`. After updating scopes, reinstall the app to your workspace.

## Code Structure

- `cmd/aws/main.go`: Entrypoint; defines `pluginName` and starts `executor.Serve` with `Executor`.
- `cmd/aws/executor.go`: Botkube interface; implements `Metadata` and `Execute` (command routing, safety checks, execution flow).
- `cmd/aws/bundle.go`: AWS CLI bundle handling; `prepareAws`, `ensureFromBundle`, `untarGzSafe` with safe extraction limits.
- `cmd/aws/utils.go`: Shared helpers and runtime utilities.
  - Helpers: `depsDir`, `httpGetToFile`, `safeJoin`, `isExecutable`, `normalizeCmd`.
  - Runtime: `resolveLoaderPath`, `buildLDPath`, `buildEnv`, `runAWS`, `listEC2InstanceIDs`.
  - Constants: extraction limits and `defaultBundleURL`.
- `cmd/aws/help.go`: Interactive help UI; `Help` returns sections and buttons, `fullHelpText` for long examples.
- `cmd/aws/config.go`: Configuration; `Config` struct, `mergeExecutorConfigs`, and allowlist check `isAllowed`.

## Getting Started

Add the plugin repository and enable the executor in your Botkube values.yaml, then bind it to a channel. Replace placeholders with your repo and credentials.

```yaml
plugins:
  repositories:
    awscli-repo:
      url: https://github.com/hskoon0722/botkube-awscli/releases/download/v0.0.1-dev/plugins-index.yaml

  executors:
    aws:
      repository: awscli-repo
      name: aws
      enabled: true
      config:
        defaultRegion: your-default-region
        # restrict which commands are allowed (prefix match)
        allowed:
          - "sts get-caller-identity"
          - "ec2 describe-instances"
          - "eks list-clusters"
          - "s3api list-buckets"
        # add global args (optional)
        prependArgs: []
communications:
  default-group:
    socketSlack:
      enabled: true
      channels:
        - name: aws-cli
          bindings:
            executors:
              - name: aws
```

Example IAM Role with Policy
Policy
```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "Identity",
      "Effect": "Allow",
      "Action": ["sts:GetCallerIdentity"],
      "Resource": "*"
    },
    { "Sid": "ComputeRead",
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeVpcs",
        "ec2:DescribeSubnets",
        "eks:ListClusters",
        "ecs:ListClusters",
        "lambda:ListFunctions"
      ],
      "Resource": "*"
    },
    { "Sid": "StorageRead",
      "Effect": "Allow",
      "Action": ["s3:ListAllMyBuckets"],
      "Resource": "*"
    },
    { "Sid": "DatabaseRead",
      "Effect": "Allow",
      "Action": [
        "rds:DescribeDBInstances",
        "dynamodb:ListTables",
        "elasticache:DescribeCacheClusters"
      ],
      "Resource": "*"
    },
    { "Sid": "RebootOnlyTaggedInstancesInRegion",
      "Effect": "Allow",
      "Action": "ec2:RebootInstances",
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "aws:ResourceTag/AllowReboot": "true"
        }
      }
    }
  ]
}
```
Trust relationships
```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Principal": {
                "Federated": "arn:aws:iam::${your_aws_account_id}:oidc-provider/${your_eks_openID_connect_provider_url}"
            },
            "Action": "sts:AssumeRoleWithWebIdentity",
            "Condition": {
                "StringEquals": {
                    "${your_eks_openID_connect_provider_url}:aud": "sts.amazonaws.com",
                    "${your_eks_openID_connect_provider_url}:sub": "system:serviceaccount:${your_botkube_namespace}:${your_botkube_service_account_name}"
                }
            }
        }
    ]
}
```

```yaml
# You can use ServiceAccount using IRSA instead of AWS ACCESS_KEY.
serviceAccount:
  create: true
  name: "botkube"
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::${your_aws_account_id}:role/${your_role_name}
```
you can refer to this url for more details : https://docs.aws.amazon.com/eks/latest/userguide/enable-iam-roles-for-service-accounts.html


Usage examples in chat:
- `aws --version`
- `aws sts get-caller-identity`
- `aws help` (help is ephemeral; only visible to the requester)

## Auxiliary Scripts

- `hack/build_aws_bundle.sh`: Builds AWS CLI runtime bundles per arch (amd64/arm64). Downloads official AWS CLI zip, copies `awscli/dist`, collects required shared libs + dynamic loader, and packages `dist/aws_linux_<arch>.tar.gz`.
- `hack/gen_plugins_index.sh`: Generates `plugins-index.yaml` from binaries in `dist/`. Uses `PLUGIN_DOWNLOAD_URL_BASE_PATH` and current tag to build URLs; fails in CI if no binary URLs are detected.
- `hack/release_gh.sh`: Creates/updates a GitHub Release and uploads assets. Reads release notes from `.github/release-notes.md` (supports `${TAG}`, `${REPO_NAME}`, `${PLUGIN_DOWNLOAD_URL_BASE_PATH}` via `envsubst` or `sed`).

## Workflows

- `.github/workflows/release.yml`: Tag-driven release pipeline. Steps: checkout → setup Go → GoReleaser build → QEMU setup (arm64) → `make aws-bundle-all` → `make gen-plugin-index` → `make release-gh` → print index URL.
- `.github/workflows/testing.yml`: On pushes/PRs, runs housekeeping (cleanup, cancel previous runs) and code quality checks via `golangci-lint`.

## Make Targets

- `build-plugins`: Build executor binaries into `dist/` via GoReleaser snapshot.
- `build-plugins-single`: Build for current GOOS/GOARCH only.
- `aws-bundle-amd64` / `aws-bundle-arm64`: Build AWS CLI runtime bundles per arch.
- `aws-bundle-all`: Build both runtime bundles.
- `gen-plugin-index`: Generate `plugins-index.yaml` from `dist/` and base URL.
- `release-gh`: Create/update GitHub release and upload assets.
- `release-all`: Build plugins, bundles, index, and publish release.
- `fmt`: Run `gofmt` locally to format Go files.

Notes:
- `gen-plugin-index` expects `PLUGIN_DOWNLOAD_URL_BASE_PATH` to be set (CI sets it automatically). For local testing you can export a dummy value.
- Release notes use a template file at `.github/release-notes.md`. You can customize it; variables available: `${TAG}`, `${REPO_NAME}`, `${PLUGIN_DOWNLOAD_URL_BASE_PATH}`.

## Release Notes Template

- File: `.github/release-notes.md`
- Environment substitution:
  - Uses `envsubst` if present; otherwise falls back to simple `sed` substitutions.
- Override file path by setting `RELEASE_NOTES_FILE` when running `make release-gh` or in CI env.
