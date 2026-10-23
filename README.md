# tforphans

[![Release](https://img.shields.io/github/v/release/5xxeu/tforphans)](https://github.com/5xxeu/tforphans/releases/latest) [![CI](https://github.com/5xxeu/tforphans/actions/workflows/ci.yml/badge.svg)](https://github.com/5xxeu/tforphans/actions/workflows/ci.yml) [![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

Finds live infrastructure that no Terraform state manages: resources created
by hand, left behind by deleted configurations or made by other tools. Then
writes the `import` blocks to bring them under Terraform.

## How It Works in 30 Seconds

1. **List what is live.** Every provider with
   [list resources](https://developer.hashicorp.com/terraform/language/block/tfquery/list)
   lists its objects through `terraform query` (Terraform 1.14+), with the
   root module's own provider configuration. Providers without them are
   listed through their plural data sources (`-scan`) or a plugin.
2. **Read every state.** The roots' own states, every root under
   `-state-roots`, and any `-state` file or URL, for every workspace.
3. **Match.** Each live object is looked up in the states by its
   [resource identity](https://developer.hashicorp.com/terraform/plugin/framework/resources/identity)
   (or by attributes for older state).
4. **Report.** What matches nothing is unmanaged: a table, JSON, or import
   blocks with `tforphans adopt`. The last line says whether the check was
   complete.

```
$ tforphans -state-roots ~/work/infra
== Unmanaged resources (2) =================================
SCOPE                    TYPE           NAME             IDENTITY                                               SOURCE
account_id 123456789012  aws_instance   debug-box        account_id=123456789012, id=i-0abc, region=eu-north-1  .

account_id 123456789012  aws_s3_bucket  tmp-export-2024  account_id=123456789012, bucket=tmp-export-2024, ...   .

== Summary =================================================
512 live, 498 managed, 0 uncertain, 12 ignored, 2 unmanaged
41 states read, 187 resource types queried, 58 skipped (-v to list)

== Coverage ================================================
coverage: complete
```

(Illustrative output.)

## Supported Providers

| Provider | How it is listed | What is covered | Used on real infrastructure |
|:---------|:-----------------|:----------------|:----------------------------|
| AWS (`hashicorp/aws`) | list resources | 247 resource types in 6.68 | not yet |
| Google Cloud (`hashicorp/google`, `google-beta`) | list resources | 152 resource types in 8.6 | yes |
| Azure (`hashicorp/azurerm`) | list resources | 118 resource types in 5.9 | not yet |
| Any other provider with list resources | list resources | whatever the installed version lists | |
| Cloudflare (`cloudflare/cloudflare` 5.x) | `-scan cloudflare`, plural data sources | about 95 of 271 resource types; the rest are mostly settings | yes |
| Proxmox VE (`bpg/proxmox`; `telmate/proxmox` in state) | `-scan proxmox`, plural data sources | VMs, containers, pools, users, groups, roles, HA, replication, SDN vnets, backup jobs, ACME accounts | yes |
| Vault (`hashicorp/vault`) | plugin [`vault.sh`](examples/plugins/vault.sh) | auth methods, roles, secrets engines, policies | |
| Junos (`jeremmfr/junos`) | plugin [`junos.py`](examples/plugins/junos.py) | interfaces, BGP, policies, prefix lists, security zones, applications | new |
| Anything else | your own [plugin](docs/plugins.md): one JSON object per line | | |

List resource coverage grows with every provider release, without changes
here: `tforphans -v` lists the types a run skipped and why. Planned next:
GitHub (`-scan github`), then plugins for Helm and Kubernetes.

## Quick Start

1. Install (macOS, Linux):

   ```sh
   brew install 5xxeu/tap/tforphans
   ```

   Other ways: [Install](#install).
2. Go to an initialized root module, with read-only credentials for its
   provider in the environment, and run it:

   ```sh
   cd infra/aws/prod && terraform init
   tforphans
   ```
3. Give it the states of every other root that manages the same accounts,
   or their objects show as unmanaged:

   ```sh
   tforphans -state-roots ~/work/infra
   ```
4. Adopt what should be in Terraform:

   ```sh
   tforphans adopt -state-roots ~/work/infra -o imports.tf
   terraform plan -generate-config-out=generated.tf
   ```

Check readiness before scanning with `tforphans doctor` (see
[diagnostics](docs/doctor.md)). Exit 0 means ready in the selected scope,
not that no orphans exist or that a future scan will succeed.

For Cloudflare or Proxmox, no root module is needed:

```sh
export CLOUDFLARE_API_TOKEN=...   # "Read all resources" template
tforphans -scan cloudflare -state-roots ~/work/cloudflare
```

## Contents

- [Quick Start](#quick-start), [Why](#why), [Features](#features), [When Not to Use It](#when-not-to-use-it), [Install](#install), [Requirements](#requirements)
- [How it works](docs/how-it-works.md): listing, reading states, matching by identity or attributes, `-refresh`
- [Output, exit status and coverage](docs/usage.md), [JSON output](docs/json.md), [config file](docs/config.md)
- [Roots, modules and states](docs/roots-and-states.md): `-state-roots`, `-state`, workspaces, Terragrunt
- [Regions, zones and other list arguments](docs/list-arguments.md): `-arg`, `zone=auto`, your own list blocks, `-types`
- [Ignoring known resources](docs/ignore.md): the ignore file and the built-in presets
- [Bringing orphans under Terraform](docs/adopt.md): `tforphans adopt`
- [Providers without list resources](docs/scan.md): `-scan cloudflare`, `-scan proxmox`
- [Plugins](docs/plugins.md): Vault, Junos, your own
- [In CI](docs/ci.md), [Security](docs/security.md), [Limitations](docs/limitations.md)

## Why

Infrastructure created outside Terraform skipped code review, policy checks
(checkov, tfsec, OPA) and cost estimates (Infracost), because all of those
look at code. tforphans finds it, which helps with:

- security: hand-made access policies, identity providers, tokens, members,
  tunnels, DNS records that may dangle (subdomain takeover);
- cost: forgotten instances, disks, IPs and other resources nobody owns;
- IaC coverage: knowing what still has to be brought under Terraform, and
  doing it with `tforphans adopt`.

It does not judge how a resource is configured (that is what CSPM tools
such as Prowler do) and knows nothing about prices; `-format json` hands
the findings to tools that do.

## Features

- 🔎 **No Provider Knowledge to Maintain**: Resource types come from the providers' own list resources, so coverage follows provider releases.
- 🗂 **Every State Counts**: `-state-roots` finds and reads every root module and workspace in a tree, through any backend Terraform supports; Terragrunt units too.
- 🪪 **Exact Matching**: Resource identity from state (Terraform 1.12+), checked against the provider that listed the object; older state is matched by attributes, and a match that cannot confirm the account or cluster is reported as uncertain, not as managed.
- ✅ **Honest Coverage**: The last line says whether "nothing unmanaged" can be trusted, with every failed listing, unread state and skipped type counted; `-strict` turns gaps into a failed CI job.
- 📥 **Adoption**: `tforphans adopt` writes `import` blocks for `terraform plan -generate-config-out`, with addresses that never collide with existing ones.
- 🧹 **Little Noise**: Objects the cloud creates on its own (default security groups, GKE nodes, managed rulesets) are hidden by default presets that go only by marks users cannot set; an ignore file covers the rest.
- 🌍 **All Regions and Zones**: `-arg region=...` fans list queries out; `-arg zone=auto` reads every Google Cloud zone.
- ☁️ **Beyond List Resources**: Plural data sources for Cloudflare and Proxmox VE, with crash isolation; plugins for anything else.
- 🔐 **Read Only**: Never applies your configuration and plans it only with `-refresh`, which writes nothing; read-only credentials are enough; no telemetry.
- 📄 **Scriptable**: Versioned JSON output, counts per type and per scope, a config file per audit.

## When Not to Use It

- **The objects belong to another tool by design** (Argo CD, Helm, Kubernetes operators, CloudFormation): they show as unmanaged, because to Terraform they are. Ignore them, or use that tool's own drift detection.
- **You want to know how resources are configured** (public buckets, open security groups): that is a CSPM tool's job; feed it `-format json` if you want both.
- **The provider has neither list resources nor plural data sources**: only a [plugin](docs/plugins.md) can list it.
- **OpenTofu without `terraform`**: OpenTofu has no `query` command yet; only `-no-query` with `-scan` and plugins can work (not tested).

## Install

Binaries for Linux, macOS and Windows are on the
[releases page](https://github.com/5xxeu/tforphans/releases), with
checksums, an SBOM and a build provenance attestation
(`gh attestation verify tforphans_*.tar.gz -R 5xxeu/tforphans`). With
Homebrew (macOS, Linux):

```sh
brew install 5xxeu/tap/tforphans
```

Or build it (Go 1.26 or newer):

```sh
go install github.com/5xxeu/tforphans@latest
```

## Requirements

Terraform in `PATH` (or `-terraform /path/to/bin`):

| What | Terraform |
|:-----|:----------|
| Listing with `terraform query` (the default) | 1.14 or newer; tforphans checks and says so |
| Matching by resource identity | state written by 1.12 or newer with a provider that records identities; older state is matched by attributes (less certain, see [`-refresh`](docs/how-it-works.md)) |
| `adopt` import blocks with `identity` | 1.12; with `id` 1.5 |
| `-no-query` with `-state`, `-state-roots`, `-scan`, `-plugin` | no query needed; tested only with 1.15 and 1.16 so far |

The tests run against 1.15 and 1.16. Roots that are queried must allow 1.14
in their `required_version` (`-ignore-required-version` overrides it for
the run); roots whose state is only read need not.

Credentials: whatever the providers and backends need; read-only is enough
(AWS `ReadOnlyAccess`, GCP Viewer, Azure Reader, a Cloudflare "Read all
resources" token, a Proxmox PVEAuditor token).

Exit status: `0` nothing found, `2` unmanaged resources found, `1` an error
or, with `-strict`, incomplete coverage. See [Output, exit status and
coverage](docs/usage.md).

## Related Tools

- [driftctl](https://github.com/snyk/driftctl) solved the same problem with
  its own scanners for every resource type; it is in maintenance mode.
  tforphans keeps no provider knowledge of its own and relies on
  `terraform query`, so coverage follows the providers.
- [tfimport](https://github.com/coolapso/tfimport) works in the opposite
  direction: you write the configuration for existing infrastructure and it
  works out the import IDs. It does not look for unknown resources.

## Development

```sh
go test ./...
go test -tags integration .   # needs terraform; builds testdata/fakeprovider
```

CI (`.github/workflows/ci.yml`) runs gofmt, `go vet`, the unit tests with
the race detector, golangci-lint, govulncheck and the integration tests
against Terraform 1.15 and 1.16. The integration tests use a fake provider
(things are files in a temporary directory) that implements list resources
and resource identity, and run the whole flow including the import round
trip; the Proxmox test runs the real `bpg/proxmox` against a fake Proxmox VE
API.

Some tests run the binary itself (this test binary as `main`): exit codes,
stdout and stderr, SIGTERM and SIGINT with plugin children, and (with
`-tags integration`) a stop during `terraform query` that must leave no
temporary file in the root. `testdata/golden` holds JSON reports of a fixed
state and plugin output; after an intended change of the format, rewrite
them with `TFORPHANS_UPDATE_GOLDEN=1 go test -run TestGoldenJSON .` and
review the diff. Every key of these reports must be described in
`docs/json.md`.

Fuzz targets check that plugin output always becomes import blocks that
parse as HCL with every id and identity value intact (`FuzzImportHCL`,
`FuzzHCLString`), that no state crashes the parsers and every identity
matches itself (`FuzzParse`), and that `terraform query` output gives the
same result however it is split (`FuzzParseQuery`). The unit tests run their
seeds; CI fuzzes each for a minute every night. Locally:

```sh
go test -run '^$' -fuzz '^FuzzImportHCL$' -fuzztime 5m ./internal/report/
```

The `e2e` workflow scans the DNS records of a real Cloudflare zone every
night (and by hand, never on pull requests) against the local state in
`testdata/e2e/cloudflare`, which holds some of them, and checks the exact
unmanaged record IDs, that the state's records are managed and that records
Cloudflare creates (Email Routing, Workers) are not reported. The token (it
only needs to read that zone's DNS) and the zone ID live in the
`cloudflare-e2e` environment. Locally:

```sh
CLOUDFLARE_API_TOKEN=... E2E_ZONE_ID=... go test -tags e2e -run '^TestE2E' -v .
```

A `v*` tag on a commit of main waits for that commit's `ci` run to pass,
then builds the release with GoReleaser (`.goreleaser.yaml`) as a draft,
with checksums, SBOMs and a build provenance attestation. Publishing the
draft writes the Homebrew cask (`scripts/cask.sh`) to `5xxeu/homebrew-tap`
(secret `HOMEBREW_TAP_TOKEN`: a fine-grained token with contents write on
that repository only); the `cask` workflow can also be run by hand for a
tag.

## License

MIT, see [LICENSE](LICENSE).
