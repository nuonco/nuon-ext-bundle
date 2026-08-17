# Nuon air-gap bundles

An air-gap bundle is a single, signed, checksummed archive that contains
everything needed to deploy an app into a customer AWS account with **zero
control-plane connectivity**: the sandbox and component plans, every pinned
container image, the CloudFormation stack templates, and the runner itself.

The vendor publishes the bundle from their Nuon org. The customer deploys it
with the `nuon-bundle` CLI using nothing but their own AWS credentials — no
Nuon API token, no callbacks to Nuon, nothing leaves their account.

```diagram
VENDOR (online)                          CUSTOMER (air-gapped AWS account)
┌───────────────────────────┐            ┌─────────────────────────────────┐
│ nuon apps sync            │            │ nuon-bundle verify / inspect    │
│ nuon apps bundles create  │  one file  │ nuon-bundle init  (once)        │
│ nuon apps bundles download│ ─────────▶ │ nuon-bundle push   → their ECR  │
└───────────────────────────┘            │ nuon-bundle stack prepare → S3  │
                                         │ aws cloudformation create-stack │
                                         │ nuon-bundle status / logs /     │
                                         │             results             │
                                         └─────────────────────────────────┘
```

## Part 1 — Vendor: define, sync, publish

Prerequisites: the `nuon` CLI authenticated against your org
(`nuon auth login`), with the app selected.

### 1. Define and sync the app

Write the app config as usual (sandbox, components, inputs) and sync it:

```bash
nuon apps sync
```

Sync waits for the scheduled component builds by default. The bundle is cut
from a specific **app config**, so note the config ID from the sync output
(or `nuon apps configs list`).

### 2. Publish a bundle

```bash
nuon apps bundles create --config-id <app-config-id>
```

This pins every image and artifact referenced by that config, renders the
offline plan envelope and stack templates, and uploads one archive. The
command waits until the bundle is `active` (or reports the publish error).

### 3. Download the archive

```bash
nuon apps bundles list
nuon apps bundles download <bundle-id> --file acme-app.oci.tar.zst
```

That one `.oci.tar.zst` file is the entire handoff to the customer. Ship it
however you ship binaries today — portal, S3 presign, physical media.

## Part 2 — Customer: deploy the bundle offline

Prerequisites:

- An AWS account with credentials that can create ECR repositories, an S3
  bucket, and CloudFormation stacks (VPC, IAM, EKS, EC2).
- The `nuon-bundle` binary.
- The bundle file from the vendor.

Everything below runs against the **customer's** account. No Nuon API token
is used at any point.

### 1. Verify and inspect the bundle

```bash
nuon-bundle verify acme-app.oci.tar.zst
nuon-bundle inspect acme-app.oci.tar.zst
```

`verify` checks every artifact and blob against the bundle's checksums.
`inspect` lists what's inside: components, pinned images, stack
templates, and the app's install inputs — so security review can happen
before anything touches the account.

The `INPUTS` table shows which inputs are `editable` offline (the vendor's
publish step replaced their values with placeholders), their defaults, and
which are required. Secrets are never shipped in a bundle and cannot be
supplied offline.

### 2. One-time setup: the deployment context

Create the S3 bucket that will hold install assets, stack outputs, and
runner state:

```bash
aws s3 mb s3://acme-nuon-install --profile customer-admin --region us-east-1
```

Then initialize the deployment context. Every later command reads its
settings from this context, so day-to-day commands are flagless. In a
terminal, `init` is an interactive form:

```bash
nuon-bundle init
```

Or write the settings to a file once and create a named context from it:

```bash
cat > acme.yaml <<'EOF'
ecr_registry: 111122223333.dkr.ecr.us-east-1.amazonaws.com
ecr_prefix: acme
bucket: acme-nuon-install
bucket_prefix: installs/
region: us-east-1
profile: customer-admin
deployment_id: acme
EOF

nuon-bundle init --name acme --file acme.yaml
```

Context settings:

| Key             | Meaning                                                        |
| --------------- | -------------------------------------------------------------- |
| `ecr_registry`  | Customer ECR registry the bundle images are pushed into        |
| `ecr_prefix`    | Repository name prefix for pushed images                       |
| `bucket`        | Customer S3 bucket for install assets, outputs, and state      |
| `bucket_prefix` | Key prefix inside the bucket                                   |
| `region`        | AWS region                                                     |
| `profile`       | AWS profile used by every command                              |
| `deployment_id` | Names this deployment; lets one bundle be installed repeatedly |

Multiple installs are managed with named contexts:

```bash
nuon-bundle ctx            # list contexts, show the active one
nuon-bundle ctx staging    # switch
nuon-bundle ctx -          # switch back
```

### 3. Push images into the customer registry

```bash
nuon-bundle push acme-app.oci.tar.zst --yes
```

Every pinned image in the bundle — including the runner image — is pushed
into the customer's ECR under `ecr_prefix`. The runner image reference is
recorded back into the context so `stack prepare` finds it.

### 4. Prepare and launch the install stack

If `inspect` showed editable install inputs, put your values in a flat
YAML (or JSON) file first — synthetic per-component overrides use the
`override:<kind>:<component>` alias shown by `inspect`:

```yaml
# inputs.yaml
app_domain: app.acme-internal.example
admin_email: platform-team@acme.example
```

```bash
nuon-bundle stack prepare acme-app.oci.tar.zst --inputs inputs.yaml --yes
```

Values are validated against the bundle's input specs (unknown names,
type mismatches, and secrets are rejected; required inputs without a
default must be present), uploaded to `config/inputs.json` under the
deployment prefix, and substituted into the frozen plans by the runner at
execution time. A run that is missing a required input fails loudly
instead of deploying the vendor's reference value.

`stack prepare` uploads the bootstrap assets, plan envelope, and
CloudFormation templates to the context's bucket, then prints the exact
`aws cloudformation create-stack` command to run. Stack names, runner IDs,
and log groups are derived deterministically from the install and
`deployment_id`, so two deployments in the same account never collide, and
re-preparing the same deployment is idempotent.

Run the printed command, then wait:

```bash
aws cloudformation create-stack --stack-name <printed-name> ...
aws cloudformation wait stack-create-complete --stack-name <printed-name> \
  --profile customer-admin --region us-east-1
```

The stack creates the VPC, IAM roles, EKS cluster, and the runner VM. When
it finishes, the stack "phones home" its outputs **to the customer's own S3
bucket** — not to Nuon:

```bash
aws s3 cp s3://acme-nuon-install/installs/acme/stack-outputs/outputs.json - \
  --profile customer-admin --region us-east-1
```

The runner boots, reads the bundled plan and the stack outputs from S3, and
works through the install on its own.

### 5. Follow the install

```bash
nuon-bundle status --follow
```

`status` shows every job in the run with its ID, phase, and outcome;
`--follow` polls until the run finishes. Drill into any job by ID:

```bash
nuon-bundle logs <job-id>
```

When the run completes, the post-trip report shows what ran, how long each
step took, and what it produced:

```bash
nuon-bundle results
```

All three commands read run state from the context's S3 bucket; pass
`--state <dir|s3://bucket/prefix>` to point somewhere else explicitly.

### 6. Watch component health (day 2)

The runner stays resident after bootstrap and keeps reporting the health
of every deployed component (and the sandbox's own releases) once a
minute, into the same S3 state prefix:

```bash
nuon-bundle health            # latest health per component + recent transitions
nuon-bundle health --follow   # stream health transitions as they happen
```

Health changes are recorded as immutable transitions, so a component that
degrades and recovers overnight is still visible the next morning. Only
one runner may own a deployment at a time: a deployment-wide lease (the
`LEASE` object next to the state) is acquired before any work and renewed
every 30 seconds, so a replaced VM whose predecessor is still alive
cannot execute the same install concurrently.

## Day-2 operations

After bootstrap, the resident runner publishes the actions, drift checks,
and runbooks available for this deployment:

```bash
nuon-bundle refs
nuon-bundle run action.restart-api
nuon-bundle run drift.api --no-wait
```

`run` writes an immutable dispatch request to the deployment state prefix
and follows the runner's claim, run steps, and terminal receipt. Use
`--dispatch-id` to supply an idempotency key from automation.

List previous runs or inspect a run's steps, job IDs, and drift verdict:

```bash
nuon-bundle runs
nuon-bundle runs <run-id>
nuon-bundle logs <job-id>
```

Start the local operator portal with a random localhost port, or choose one:

```bash
nuon-bundle portal
nuon-bundle portal --port 8080 --requested-by platform-team
```

The portal shows component health, dispatchable refs, and expandable run
history. AWS access remains in the CLI process; the portal never edits or
constructs plans.

## What never happens on the customer side

- No Nuon API token is created or used.
- No traffic leaves the customer account for Nuon: images come from their
  ECR, plans and state live in their S3, stack outputs phone home to their
  bucket.
- The control plane cannot see, affect, or react to the install.

## How plans stay correct without the control plane

Bundles are compiled from the app config alone — no reference install —
with placeholder tokens for every environment-specific value, and the
offline runner late-binds those tokens from local ground truth just before
each job executes. See
[pkg/runner/airgap/ARCHITECTURE.md](../../pkg/runner/airgap/ARCHITECTURE.md)
for how compilation and late binding work, and how this differs from the
connected BYOC flow.
