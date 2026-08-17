# nuon-ext-bundle

Nuon CLI extension for deploying and operating [Nuon air-gap bundles](https://github.com/nuonco/nuon) offline.

Once installed, the extension is available as `nuon bundle`. It is the customer-side counterpart to the
vendor-side `nuon apps bundles` commands: vendors create and publish bundles with the Nuon control plane;
customers use `nuon bundle` to verify, inspect, and deploy those bundles into their own cloud account with
no connectivity back to Nuon.

## Install

```sh
nuon ext install bundle
```

No Nuon API token or org is required — everything runs against the bundle archive and the customer's own
AWS account.

## Commands

| Command | Purpose |
| ------- | ------- |
| `nuon bundle init` | One-time interactive (or file-based) setup: ECR repo, S3 bucket/prefix, AWS profile |
| `nuon bundle verify` | Verify a bundle archive's integrity and signature |
| `nuon bundle inspect` | Show a bundle's contents: components, actions, runbooks, inputs |
| `nuon bundle push` | Push bundle images/artifacts into the customer's ECR/S3 |
| `nuon bundle stack` | Prepare and manage the CloudFormation stack that hosts the runner |
| `nuon bundle deploy` | Register the deployment and start the install workflow |
| `nuon bundle status` | Follow workflow/job status for a deployment |
| `nuon bundle logs` | Fetch job logs by job ID |
| `nuon bundle runs` / `run` | List and trigger day-2 runs (actions, runbooks, drift checks) |
| `nuon bundle results` | Show job results |
| `nuon bundle refs` | Show catalog refs available for day-2 operations |
| `nuon bundle health` | Show component health |
| `nuon bundle portal` | Serve the local day-2 operations portal |
| `nuon bundle ctx` | Manage saved bundle contexts |

The same binary also works standalone (without the `nuon` CLI) as `nuon-ext-bundle`; air-gapped hosts that
cannot reach GitHub receive it inside the bundle itself.

## Development

The CLI source lives in this repository (`main.go` + `cmd/`). Shared platform machinery — the app-config
compiler, plan generation, and the air-gap envelope/state contract also consumed by the runner and
control plane — is imported from the [`github.com/nuonco/nuon`](https://github.com/nuonco/nuon) Go module
at a pinned version in [`go.mod`](go.mod).

```sh
make build
nuon ext install .
```

To develop against unpushed monorepo changes, add a local replace:

```sh
go mod edit -replace github.com/nuonco/nuon=/path/to/nuon
```

### Bumping the monorepo dependency

```sh
go get github.com/nuonco/nuon@<commit> \
       github.com/nuonco/nuon/sdks/nuon-go@<commit> \
       github.com/nuonco/nuon/sdks/nuon-runner-go@<commit>
go mod tidy
```

The replace directives in `go.mod` mirror the monorepo's own replace block (replaces do not propagate to
consumers); re-check them against the monorepo `go.mod` when bumping.

## Releasing

Tag `vX.Y.Z` and push the tag; the release workflow tests, cross-compiles
`nuon-ext-bundle-<os>-<arch>` for linux/darwin × amd64/arm64, and attaches the binaries to the GitHub
release, where `nuon ext install` picks them up.
