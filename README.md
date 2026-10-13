# tforphans

Finds live infrastructure that no Terraform state manages: resources created
by hand, left behind by deleted configurations or made by other tools.

It works with any provider that supports
[list resources](https://developer.hashicorp.com/terraform/language/block/tfquery/list)
(Terraform 1.14+). Recent `hashicorp/aws`, `hashicorp/google` and
`hashicorp/azurerm` list hundreds of resource types each, and the coverage
grows with every provider release without changes here. Providers without
list support are covered through their plural data sources (Cloudflare and
Proxmox so far) and by plugins.

## Why

Infrastructure created outside Terraform skipped code review, policy
checks (checkov, tfsec, OPA) and cost estimates (Infracost), because all of
those look at code. tforphans finds it, which helps with:

- security: hand-made access policies, identity providers, tokens, members,
  tunnels, DNS records that may dangle (subdomain takeover);
- cost: forgotten instances, disks, IPs and other resources nobody owns;
- IaC coverage: knowing what still has to be brought under Terraform, and
  doing it with `tforphans adopt`.

It does not judge how a resource is configured (that is what CSPM tools such
as Prowler do) and knows nothing about prices; `-format json` hands the
findings to tools that do.

## How it works

For every root module directory:

1. `terraform providers schema -json` tells which resource types the
   module's providers can list.
2. A temporary `zz_tforphans_<pid>.tfquery.hcl` with one `list` block
   per type is written next to the configuration, `terraform query -json`
   runs with the module's own provider configuration and credentials, and
   the file is removed.
3. `terraform state pull` gives the module's state, for every workspace
   (selected through `TF_WORKSPACE`, so the module's selected workspace
   stays as it is). Workspaces are taken only from the list lines of
   `terraform workspace list` (`* name`, `  name`), never from other text
   such as backend warnings: pulling a workspace that does not exist makes
   some backends (S3) write an empty state. `-current-workspace` skips the
   list and reads only the selected workspace.

Every listed resource is then compared by
[resource identity](https://developer.hashicorp.com/terraform/plugin/framework/resources/identity)
with all states that were read: the roots' own states plus any `-state`.
A state instance counts only when it belongs to the provider that listed the
object (by provider source; google and google-beta count as one): two
providers can define a type of the same name for different objects.
State written before identities existed (or by providers without them) is
matched by attributes instead; every key of the object's own ID has to
match. Old state often lacks the account or subscription (AWS `account_id`
is no attribute), and so can an identity, so such a match cannot tell an
IAM role `ci` in one account from a role `ci` in another. An empty value in
a listed identity (google lists some objects with `project = ""`) is
unknown too, and such objects from two roots are reported separately, since
they may live in two projects. These matches are reported as uncertain,
not as managed (an error with `-strict`), and a note says how many state
instances have no identity;
`-refresh` reads each root's state through `terraform plan -refresh-only`
instead of `state pull`: the providers return identities for old state,
nothing is written and no lock is taken (the plan file lives in a private
temporary directory and is removed). It costs a full refresh of every root
it reads, `-state-roots` included: every resource is read from the API with
the root's credentials (time and API quota; 1-2 minutes for a root of a few
hundred resources is common) and the root's variables are needed. A line on
stderr says when each root starts and how long it took. When the plan
fails, the stored state is read and a note says so. Identities matter only
for state written before Terraform 1.12; such state is matched by
attributes otherwise, which found the same orphans in the runs so far.
`terraform apply -refresh-only` with a current provider records identities
in state for good (it writes the state, so it needs write access). The boot disk of a managed `google_compute_instance` counts as
managed by it; disks attached with `attached_disk` do not, since they may
be created elsewhere. Whatever matches nothing is reported.

## Install

Binaries for Linux, macOS and Windows are on the
[releases page](https://github.com/5xxeu/tforphans/releases), with
checksums, an SBOM and a build provenance attestation
(`gh attestation verify tforphans_*.tar.gz -R 5xxeu/tforphans`). With
Homebrew (macOS, Linux; from 0.1.1):

```sh
brew install 5xxeu/tap/tforphans
```

Or build it (Go 1.26 or newer):

```sh
go install github.com/5xxeu/tforphans@latest
```

Terraform in `PATH` (or `-terraform /path/to/bin`). What needs which
version:

| What | Terraform |
|---|---|
| Listing with `terraform query` (the default) | 1.14 or newer; tforphans checks and says so |
| Matching by resource identity | state written by 1.12 or newer with a provider that records identities; older state is matched by attributes (less certain, see `-refresh`) |
| `adopt` import blocks with `identity` | 1.12; with `id` 1.5 |
| `-no-query` with `-state`, `-state-roots`, `-scan`, `-plugin` | no query needed; tested only with 1.15 and 1.16 so far |

The tests run against 1.15 and 1.16. Roots that are queried must allow
1.14 in their `required_version` (`-ignore-required-version` overrides it
for the run); roots whose state is only read need not. OpenTofu has no
`query` command yet; with it only `-no-query` mode can work (not tested).

## Usage

```sh
cd infra/aws/prod && terraform init
tforphans                     # same as: tforphans .
```

```
SCOPE                    TYPE           NAME             IDENTITY                                                SOURCE
account_id 123456789012  aws_instance   debug-box        account_id=123456789012, id=i-0abc, region=eu-north-1   .
account_id 123456789012  aws_s3_bucket  tmp-export-2024  account_id=123456789012, bucket=tmp-export-2024, ...    .

512 live, 498 managed, 0 uncertain, 12 ignored, 2 unmanaged; 1 states read, 187 resource types queried, 58 skipped (-v to list)
managed objects matched by identity: 470, by attributes: 28
coverage: complete
```

(Illustrative output.)

Exit status: `0` nothing found, `2` unmanaged resources found, `1` error.
A run that queried no resource type at all is an error too, since it could
not have found anything (for example a provider version without list
resources; the error names such providers). With `-strict`, incomplete
coverage (a failed list query, a plugin error, a type skipped for want of
an argument) is also an error, which suits CI. A root given as an argument
that is not initialized and fails for it is not queried and its state not
read: a warning names it, coverage is incomplete, and the other roots still
run (`-init` runs `terraform init` there first).

Queries that did not list everything are summed up by cause: crashed,
missing argument, permission denied, truncated, not found, request rejected
(often a feature the plan does not include), API retired, provider bug, API
not enabled, other. Messages are cut to one line; `-v` prints each failed
query.

Every live object ends up in one of four groups: **managed** (matched to
state, its account or cluster confirmed), **uncertain** (matched to state,
but state cannot say whether the instance is in the same account or
cluster; see below), **ignored** and **unmanaged**. Uncertain objects are
not counted as managed and get no import block; the text output lists them
after the unmanaged ones with the state address they matched. The second
line says how the managed objects were matched: by the resource identity in
state (exact) or by attributes.

The last line says whether "nothing unmanaged" can be trusted: `coverage:
complete`, or `coverage: INCOMPLETE` with what was not checked (failed or
truncated listings, unread states, plugin errors, and types that needed a
check and were skipped in a root: a list resource with a required argument
not given with `-arg`, a provider configured only with aliases). A type
left out on purpose (`-exclude-types`, opt-in types, google-beta listed
through google, a scope not asked for) or one with no list resource or
data source at all does not make coverage incomplete; the line then reads
`coverage: complete for what can be listed; not checked: ...`, and `-v`
lists those types. A type listed in one root does not cover a root where
it was skipped, since the roots may look at different accounts. Before the slow part,
a `plan:` line says what the run checks: which roots are queried, how many
states are read, what is scanned and with which scope.

### Config file

Flags can live in `.tforphans.yaml` in the current directory, so that a run
needs no long command line. Keys are flag names without the dash; a list
repeats a flag; `roots` lists the root modules:

```yaml
roots: [.]
state-roots:
  - stacks
  - teams/platform
scan: [cloudflare]
scan-scope: [state]
ignore-preset: [cloudflare-system]
exclude-types: [cloudflare_api_token, cloudflare_account_member]
strict: true
format: summary
```

The command line wins: a flag given there replaces the file's value, lists
included (`-scan proxmox` replaces `scan: [cloudflare]`). Relative paths are
relative to the file, `~/` is the home directory. `-config FILE` reads
another file (one per audit: `cloudflare.yaml`, `aws.yaml`), which may
also point outside the current directory (`state-roots:
[~/work/infrastructure]`); `-no-config` ignores `.tforphans.yaml`.
Credentials stay in the environment, as without a file.

Config files live in repositories and change in pull requests that a CI
job may then run, so what a file may set depends on what it could do:

| Keys | `.tforphans.yaml` | `-config FILE` |
|---|---|---|
| `plugin`, `terraform`, `terragrunt`, `allow-http`, `o`, `state` URLs | no | no |
| `init`, `refresh`, `allow-unpinned`, `include-attrs`, `state` files, `proxmox-cluster-states`, `plugin-cluster-states`, `allow-incomplete` | no | yes |
| everything else (`roots`, `state-roots`, `types`, `scan`, `scan-scope`, `ignore`, `strict`, ...) | yes | yes |

The first row runs any command, sends credentials to an address of the
file's choosing or overwrites any file: it belongs on the command line,
where a change shows in the command itself. The second runs the roots'
data sources or providers, prints attribute values, or makes results look
more certain than they are (a state file of its own counts as proof of
management): fine in a file you name, not in one that is read on its own
and may come with someone else's repository.

The paths of `.tforphans.yaml` (`roots`, `state-roots`, `ignore`) must
exist and stay within the current directory, resolved as the system does
(symbolic links included): no `..`, absolute paths or `~/` leading out of
it. A file given with `-config` may point anywhere. The `plan:` line names
the config file read. These limits are no sandbox: like the repository
itself, whose roots, states and `.tforphans-ignore` decide the result
anyway, the file can still choose which of its roots and `-state-roots` are
read. Run tforphans only in repositories you trust as much as their
Terraform states, or pass `-no-config`.

### Resources in modules

State holds every instance of the whole module tree (`module.a[0].module.b.aws_instance.x["k"]`),
so resources declared in child modules, at any depth and with `count` or
`for_each`, are matched like root ones.

Listing happens from the root module. If a provider is declared only in
child modules, a temporary `zz_tforphans_<pid>_override.tf` gives it a local
name (`tfo-<namespace>-<type>`) for the run (an override file, so it merges with the
root's own `required_providers`); no `terraform init` is needed. The listing
then uses the root's default provider configuration, which is what child
modules inherit anyway. A legacy module that configures its provider inside
itself is the exception: put a matching `list` block in your own
`*.tfquery.hcl` for it. Import blocks for such providers carry a comment,
because the root has to declare the provider (or the block has to move into
the module) before Terraform can import.

### Several roots and states

A resource counts as managed when any state holds it. Every state that
manages the same accounts has to be read, otherwise resources from the other
roots show up as unmanaged. The simplest way is to point `-state-roots` at
the checked-out repositories:

```sh
tforphans infra/aws/prod -state-roots ~/work/infra -state-roots ~/work/platform
```

`-state-roots` finds every root module in the tree (a directory with a
`backend` or `cloud` block, or local state in `terraform.tfstate` or
`terraform.tfstate.d`; child modules
and hidden, `vendor` and `testdata` directories are skipped) and reads the state of each of its
workspaces with `terraform state pull`. Terraform does the reading, so any
backend works: S3, GCS, azurerm, GitLab/http, HCP Terraform. Up to 8 roots
are read in parallel. A root that is not initialized, or whose state cannot
be read, is reported as a warning (an error with `-strict`), because its
resources may then show up as unmanaged; the last line names each unread
root with the line of the error that says why, and so does
`summary.unread_states` (`[{"root": ..., "error": ...}]`). A workspace that
fails does not drop the others: their states still count, and the warning
names the workspace. Directories that cannot be read are skipped with a
warning. More than `-max-roots` root modules (200 by default) stop the run
before terraform starts in any of them: a wrong path (`..` relative to a
config file somewhere else) can otherwise reach a whole home directory. `-init` runs `terraform init` in
uninitialized roots first; it creates `.terraform` there and may write a
lock file, so it is opt-in. A shared `TF_PLUGIN_CACHE_DIR` makes it fast.
Reading a root needs working credentials for its backend (S3, GCS, ...);
a root whose backend rejects them is skipped with a warning.

States can also be given one by one:

```sh
tforphans infra/network infra/apps \
  -state https://gitlab.example.com/api/v4/projects/18/terraform/state/legacy \
  -state old.tfstate
```

`-state` takes a file, `-` for stdin, or an https URL. URLs are read with
`TF_HTTP_USERNAME` and `TF_HTTP_PASSWORD` (or the URL's user info), like
Terraform's http backend (GitLab managed state included), so these
credentials go to every URL given. Plain http needs `-allow-http` and never
carries credentials, since state read over it could be altered in transit.
State must be raw state (`terraform state pull`), not `terraform show -json`.

### Terragrunt

`-terragrunt` makes `-state-roots` read Terragrunt units too: every
directory with a `terragrunt.hcl`, such as `live/prod/app` under a
`root.hcl`, except a parent: one with no `terraform` block, no `include`
and no `.tf` files of its own that a unit includes, with
`find_in_parent_folders()` or a literal path (a legacy top-level
`terragrunt.hcl`). A parent included some other way is taken for a unit
and shows as an unread state. The other way round, a unit that other units
include as well, with all its configuration generated (no `terraform`
block, no `.tf` files in a fresh checkout), cannot be told from a parent:
it is taken for shared configuration, a `note:` names it, and
`-terragrunt-unit DIR` (`terragrunt-unit:` in a config file) reads it as a
unit. tforphans never runs terragrunt to tell the two apart. Each unit's state is read with
`terragrunt run -- state pull` (Terragrunt 0.73 or newer in `PATH`; tested
with 1.1.6), which renders the backend the unit's configuration describes,
so remote state blocks, `generate` and includes all work. Terragrunt
initializes a unit before reading it: it fetches the module source and
providers into `.terragrunt-cache` next to `terragrunt.hcl`, as any
terragrunt command does. Terragrunt also evaluates the units' HCL, which
can run commands (`run_cmd`, hooks): the flag is opt-in and accepted only
on the command line, like `-plugin`. It runs the terraform of
`-terraform`. A `terragrunt.hcl` that cannot be read or parsed is an
unread state (coverage incomplete, `adopt` refuses), not a missing unit. A
unit cannot be a root argument as well: terraform query would read the
backend of its `.tf` files, not the one terragrunt renders.

```sh
tforphans -no-query -scan cloudflare -state-roots live -terragrunt
```

Units are read as states only: their modules are not queried, and `-refresh`
does not apply to them. Workspaces are not used by units and are not
listed.

### Regions, locations and other list arguments

List queries use the provider configuration of the root module, so by
default AWS is listed in the provider's region only. `-arg` fans every list
query whose config has that string argument out over several values:

```sh
tforphans -arg region=eu-north-1,eu-central-1,us-east-1
tforphans -arg location=global,europe-north1   # google
```

An argument can be limited to some types with `TYPE_GLOB:`, and then wins
over the same argument without one for those types; the values of several
such arguments that match a type are combined. Google Compute
instances, disks and snapshots need a zone, which the list config does not
require (the query fails with "Cannot determine zone" unless the provider
block sets one), so the first scan of a GCP project usually needs:

```sh
tforphans -arg location=global,europe-west1 \
  -arg "google_compute_*:zone=$(gcloud compute zones list --filter='region:europe-west1' --format='value(name)' | paste -sd, -)"
```

Types whose list config has a required argument that no `-arg` supplies (for
example `rest_api_id` for API Gateway children) are skipped, because one
invalid list block makes Terraform reject the whole query. `-v` lists them
with the reason.

When a root uses both `google` and `google-beta`, a type both can list is
queried once, through the provider the root declares, preferring `google`.
Types whose lists reach beyond what a root manages (`google_project` lists
every project the credentials can see) are skipped unless `-types` names
them exactly. `-v` shows, per root, which projects and accounts the results
came from.

A root pinned to an older Terraform (`required_version = "= 1.10.5"`)
refuses `terraform query`; `-ignore-required-version` lifts the root
module's constraint for each command with a temporary
`zz_tforphans_<pid>_version_override.tf` (child modules' constraints still
apply, and an override file of yours that sorts after it is an error, since
it could set the constraint again).

For anything more specific (provider aliases, other accounts, filters), put
your own `list` blocks in a `*.tfquery.hcl` file in the root module. They run
together with the generated ones and their results are checked the same way.

Generated blocks always use the provider's default configuration. A root
that configures a provider only with aliases (`provider "aws" { alias =
"prod" }` and no block without an alias) gets no generated blocks for it,
since the empty default configuration could reach another account through
the environment; a note says so, and your own list blocks with `provider =
aws.prod` cover it. Until they do, its types count as not checked in that
root (coverage incomplete, an error with `-strict`); `-exclude-types`
leaves them out on purpose. A provider with no provider block at all is queried
(credentials from the environment, as for any run).

### Narrowing the scan

```sh
tforphans -types 'aws_s3_*,aws_iam_*'
tforphans -exclude-types 'aws_cloudwatch_log_*'
tforphans -print-query      # show the generated .tfquery.hcl and exit
```

### Ignoring known resources

`.tforphans-ignore` in the working directory (or `-ignore FILE`)
holds `TYPE_GLOB[:NAME_GLOB] [KEY=GLOB...]` lines. `*` matches any
characters. The name glob is tried against the display name, the import ID
and every identity value. Each `KEY=GLOB` must match as well: an identity
key (`project=prod-*`), or `attr.` and a path into the resource's
attributes (`attr.labels.goog-gke-node=*`), which tforphans then fetches for
that rule's types (`include_resource`, slower) but does not print. Your
own list blocks need `include_resource = true` for that (a note says so).
Results of `-scan` carry the fields their data source returns; plugin
results have no attributes:

```
# AWS-managed service roles
aws_iam_role:AWSServiceRole*
# created by the platform team's own tooling
aws_s3_bucket:platform-*
# everything of a type
aws_default_vpc
# one project only
google_compute_disk name=pvc-* project=data-prod
```

`-ignore-preset gcp-system` adds built-in rules for objects that Google
Cloud and GKE create on their own, by marks a user cannot give their own
objects: GKE nodes and disks by their reserved `goog-` labels, default
service accounts and App Engine buckets in Google's own domains. See
[the preset](internal/report/presets/gcp-system.ignore). Other system
objects are known only by name (`gke-*` and `k8s-*` firewall rules and load
balancer parts, `gcf-sources-*` and `*_cloudbuild` buckets), and anyone who
can create such an object can choose that name, so the preset leaves them
out. Add them to your own ignore file if that risk is acceptable, ideally
scoped to a project:

```
google_compute_firewall name=gke-* project=my-gke-project
google_compute_firewall name=k8s-* project=my-gke-project
google_storage_bucket name=gcf-sources-*
```

`-ignore-preset cloudflare-system` is the same for `-scan cloudflare`; see
[Cloudflare](#cloudflare) for what it covers.

### Bringing orphans under Terraform: `tforphans adopt`

`adopt` takes the same flags and finds the same resources, but writes an
import block for each one instead of a report:

```sh
tforphans adopt -o imports.tf          # or to stdout without -o
terraform plan -generate-config-out=generated.tf
```

Import blocks use resource identity (Terraform 1.12+) and name the provider
when its local name is not the type prefix; objects found by your own list
blocks keep that block's provider (an alias such as `aws.other_account`). `-o` never overwrites an
existing file. Resource names avoid addresses that the states or the root
modules' configuration already use (`web` becomes `web_2`), so an import
never lands on someone else's resource block. Import blocks already in a root
module (an earlier `adopt` not applied yet, also into child modules) keep
their addresses, and objects they target are not written again: same type,
same provider configuration (`provider = vault.other` is not the default
one), the same literal `id` or `identity`, and, for objects listed by a root
module, a block in that root. Objects found by `-scan` or a plugin have no
root, so a block in any root with their provider configuration counts. With several roots or plugins the blocks are grouped under
`# Found by <source>` comments: each group belongs in its own root module.
Narrow down what to adopt with `-types` or the ignore file, and review
`generated.tf` before applying.

The file starts with a comment saying how many states it was checked
against, how many uncertain objects were left out and which types were not
fully listed (objects of those types may be missing). Import blocks of
scoped objects say where the object lives (`# in zone example.com`). When a
state could not be read, `adopt` writes nothing: its objects would show as
unmanaged and be imported a second time. Fix the access, or pass
`-allow-incomplete`.

Terraform before 1.16 generates resources without their `provider` and
accepts `provider` in an import block only while the configuration is still
to be generated: after `-generate-config-out`, move the `provider` line of
each import block into its generated resource before applying.

Generated configuration holds what the provider reads back from the API.
Some resources need more than that: write-only or secret arguments
(passwords, API tokens), content the API does not return (the code of a
`cloudflare_workers_script`, files uploaded once), or arguments that only
shape creation. `terraform plan` after the import shows them as changes or
missing values; fill them in by hand before applying.

`adopt` exits 0 whether or not it found anything (1 on errors, and with
`-strict` on incomplete coverage); the plain command is the one for CI
checks.

`-format json` prints the orphans (`orphans`), the uncertain objects with
the state address they matched (`uncertain`) and the summary: counts per
type (`summary.by_type`) and per scope (`summary.by_scope`: a zone or
account for `-scan cloudflare`, the project, account or subscription of
list results), skipped types, failed queries with their `category` and
`scope`, states not read (`summary.unread_states`) and whether the run
checked everything (`summary.complete`). `summary.coverage` has one row per
type and scope:

```json
{"type": "cloudflare_dns_record", "scope": "zone example.com", "status": "complete",
 "live": 12, "managed": 10, "uncertain": 0, "ignored": 0, "orphans": 2}
{"type": "cloudflare_dns_record", "scope": "zone example.org", "status": "incomplete",
 "reasons": ["permission denied"], "live": 0, ...}
```

`status` is `complete`, `incomplete` (with the failure categories in
`reasons`, or `skipped: <reason>` and a scope `root <dir>` for a type that
needed a check there) or `skipped` (left out on purpose or cannot be
listed; `reasons` says why). Every scope that was read has a row, also one
without objects of the type. A row without `scope` stands for the type as
a whole: a type listed without results, or a failure that concerns every
scope. Only listings that ran count: a list block with `for_each = {}`
checks nothing. Each orphan has a `scope`
such as `zone example.com` when it is known. Every orphan has the same keys,
so some are empty by design: `import_id` is empty when the import block uses
the resource identity (everything `terraform query` lists; `adopt` writes
`identity = {...}` then), and `provider` is empty for the provider's default
configuration (it holds an alias such as `aws.prod` otherwise). `-include-attrs
labels,creation_timestamp` adds those attributes to each orphan (to see how
old an object is, or who owns it); attributes can hold secrets, so ask only
for what you need. `-format summary` prints only tables of
counts per type (with the coverage of each type) and per scope, those with
unmanaged objects first, and the types not fully listed by cause.

## Providers without list support: plural data sources

Many providers that have no list resources yet have plural data sources
(`cloudflare_dns_records` next to `cloudflare_dns_record`), which list
objects with the provider's own API code and authentication. `-scan NAME`
uses them:

```sh
export CLOUDFLARE_API_TOKEN=...   # read-only, "Read all resources" template
tforphans -no-query -scan cloudflare -state-roots ~/work/cloudflare
```

For each resource type with a plural data source, a temporary root module
gets a data block (iterating over the parent zones or accounts when the
data source takes `zone_id` or `account_id`), and `terraform apply
-refresh-only` reads them into a local state in a temporary directory. Data
sources only read, nothing is changed. Credentials come from the provider's
environment variables, since the temporary root has no provider block. A
data source that returns as many objects as its `max_items` (1000000) may
have stopped there: its type counts as not fully listed, and so does every
type listed per zone or account of a parent list that hit the cap.

The provider version is the user's: the lock file and the installed
provider of the first root (given or found under `-state-roots`) that pins
the provider are reused (the data directory is `TF_DATA_DIR` when set); if
that root has the lock file but no installed provider, exactly the locked
version is downloaded and checked against the lock file's checksums (only
the provider's own lock entry is used, with `-lockfile=readonly`). Import
blocks use the provider's local name in that root (`provider = cf` for
`cf = { source = "cloudflare/cloudflare" }`), but the objects were found
with the provider configured from the environment, and the root's own
provider block may point to another account or cluster. Each such import
block says so in a comment: check it before `terraform plan`. Without such a root the scan stops: a provider
release nobody chose would run with your credentials. `-allow-unpinned`
uses the newest release instead, cached under the user cache directory.

A data source that fails (missing token permission, a product the account
does not have, a provider bug) is reported as "not fully listed" and the
others are read again, so one failure does not stop the scan. A failure in
one zone or account only takes that zone or account out; the others are
still listed. When the provider or terraform crashes (a panic names no
data source), the data sources are read again in halves until the crash
is pinned to a data source and, for those read per zone or account, to the
zones or accounts that crash; those are reported as "crashed" and the rest
is listed as usual. This takes extra runs (at most 80; what is still
suspect after them is reported as crashed), and a note says so while it
happens. With
`-strict` such gaps are errors; leave out products you do not use with
`-exclude-types`.

An adapter per provider holds what the schema cannot say. For Cloudflare
(5.x) that is:

- objects Cloudflare creates and owns are not reported, judged only by
  fields users cannot set (a name or a flag a user can set could hide a
  hand-made object): records of Workers custom domains and Email Routing,
  Cloudflare's managed rulesets (by their global IDs), Universal SSL
  certificates, managed Gateway certificates, built-in DLP entries, the
  default device profile, the catch-all rule. The default virtual network
  and the Gateway CA mTLS certificate are reported; ignore them if wanted;
- objects listed in several scopes or under several types are reported once;
- 4.x type names (`cloudflare_record`, `cloudflare_tunnel`, ...) and
  `cloudflare_worker` next to `cloudflare_workers_script` count as managing
  the same object.

`-scan proxmox` covers `bpg/proxmox` (credentials from `PROXMOX_VE_ENDPOINT`,
`PROXMOX_VE_API_TOKEN` and the other `PROXMOX_VE_*` variables; a token with
the PVEAuditor role is enough): VMs, containers, pools, users, groups,
roles, HA groups and resources, replication jobs, SDN vnets, backup jobs and
ACME accounts. Guests are matched by VM ID, which is unique in a cluster
and survives migration; `telmate/proxmox` resources
(`proxmox_vm_qemu`, `proxmox_lxc`) and the deprecated
`proxmox_virtual_environment_*` names of renamed types count as managing the
same objects. Built-in roles and `root@pam` are not reported. Import IDs
are `<node>/<vm_id>` for guests and the object ID otherwise.

Proxmox state does not record which cluster an object belongs to, and VM
IDs repeat across clusters. A guest never matches state whose node
(`node_name`, telmate's `target_node`) the scanned cluster does not have.
A node it does have proves nothing, though: node names such as `pve1`
repeat across clusters too. So all Proxmox matches are uncertain (an error
with `-strict`) unless you state that every state given belongs to the
scanned cluster:

```sh
tforphans -no-query -scan proxmox -state-roots ~/work/proxmox/cluster-a \
  -proxmox-cluster-states -strict
```

Scan one cluster per run, with that cluster's states only.

### Cloudflare

State is matched by `id` plus `zone_id` or `account_id`. Import IDs follow
the provider's convention (`<zone_id>/<id>`, or `accounts/<account_id>/<id>`
for resources that can live in an account or a zone).

**Coverage.** A type is listed when the provider has a plural data source
for it that needs no argument beyond `zone_id` or `account_id`. On a small
account that is about 95 of the 271 resource types (cloudflare 5.x), read
in under a minute; most of the rest are settings and singletons (zone
settings, `*_settings`, one-per-zone objects), which cannot be orphaned, or
child objects whose data source needs a parent ID (list items, access
policies of one application). `-v` lists every skipped type with the
reason, and `summary.skipped_types` in JSON.

**Token.** The scan reads `CLOUDFLARE_API_TOKEN` (or the provider's other
environment variables), never the root's provider block: that block can
use files, variables and functions that only a plan of your configuration
evaluates, and tforphans does not plan your configuration. If your
configuration reads the token from a file, pass it the same way:

```sh
CLOUDFLARE_API_TOKEN="$(jq -r .api_token ~/.cloudflare.json)" \
  tforphans -no-query -scan cloudflare -state-roots ~/work/cloudflare
```

Create the token from the "Read all resources" template (Account and Zone
read permissions on all accounts and zones). Types the token cannot read
are reported as "permission denied" with their names; Cloudflare's 403
does not say which permission was missing, and the permission names do not
map one to one onto resource types, so add the read permission of the
product in question (for example "Logs Read" for `cloudflare_logpush_job`,
"API Tokens Read" for `cloudflare_api_token`). Products the account's plan
does not include usually answer "400 Bad Request" ("request rejected"),
and retired APIs (the old firewall rules and filters) "410 Gone"
("API retired"); exclude such types with `-exclude-types`.

**Several accounts.** One token, one account set: run tforphans once per
account (staging, prod), each with its own token and the roots of that
account under `-state-roots`. Objects of an account whose state was not
read show up as unmanaged.

**Scope.** The scan lists whole accounts. Compared with the state of one
stack, everything that other stacks manage is reported too, so give all
states with `-state-roots`, or limit the scan:

```sh
tforphans -no-query -scan cloudflare -scan-scope zone_id=example.com,example.org -scan-scope account_id=
tforphans -no-query -scan cloudflare -scan-scope state -state-roots stacks/web
```

The zone and account limits are independent: `zone_id=...` alone still
lists the account-level objects of every account, and `account_id=A` does
not limit the zones to those of account A. The first line above lists only
the two zones (`account_id=` with no value leaves out account-level
objects).

`-scan-scope zone_id=...` (or `account_id=...`) takes IDs or names and lists
only those zones (or accounts): their objects, and the zones themselves.
`-scan-scope state` takes the zone and account IDs found in the states
read; a scope with no ID in the states is not read at all (its types are
reported as skipped). A zone stack whose state names its account (an
account-level object, or any `account_id`) gets the account's objects
listed as well; `-scan-scope account_id=` (no value) leaves out
account-level objects, and works with `state` for the zones:
`-scan-scope state -scan-scope account_id=`. Some zone listings return
account objects too (Access applications); the account listing of such a
data source is still read, only to drop them, and if it fails the type
counts as not fully listed.
Without `-scan-scope` and `-state-roots` a note points this out. Every
object carries its zone or account (`scope` in JSON, a column in the
table), and `-format summary` counts them per zone.

**Known noise.** Objects Cloudflare creates are left out by fields users
cannot set (see above), and also the resource library catalog
(`cloudflare_zero_trust_resource_library_application` with
`application_source = "cloudflare"`). `-ignore-preset cloudflare-system`
also hides the URL normalization entrypoint ruleset (created by
`cloudflare_url_normalization_settings`). Reported on purpose, ignore them
yourself if you accept them: the default virtual network and the Gateway CA
mTLS certificate (their marks are settable by users), and "default"
entrypoint rulesets and notification policies, which the dashboard creates
when someone configures rules or alerts there (configuration outside
Terraform), for example `cloudflare_notification_policy:Default*`.

**Crashes.** Some provider versions return values that crash terraform for
some objects (for example DNS records of mixed types in one list). Such a
crash costs only the zones it happens in (see above); report it to the
provider with the message from `-v`.

## Plugins: other providers without list support

A plugin is any command that prints one JSON object per line:

```json
{"type": "vault_policy", "identity": {"id": "ci"}, "display_name": "ci", "import_id": "ci"}
{"error": "sys/auth: permission denied"}
```

- `type`: Terraform resource type.
- `identity`: attributes to match. Against state without resource identity
  they are compared with the instance's attributes, so `{"id": ...}` with the
  value the provider stores as `id` is usually enough. All keys must match.
- `import_id`: optional; import blocks then use `id = ...`.
- `provider`: optional provider configuration for the import block
  (`vault.other`); `provider_source`: optional provider address
  (`registry.terraform.io/hashicorp/vault`), so that state of another
  provider with a type of the same name does not match. A `type` or
  `provider` that is not a valid name makes the plugin fail.
- `error` lines are reported as warnings (errors with `-strict`).
- `unverified`: optional, `true` when state cannot confirm where the object
  lives (Vault state does not record the Vault address, so a policy `ci`
  in state of another Vault would match). Such objects match only as
  uncertain (an error with `-strict`) unless `-plugin-cluster-states`
  states that every state given belongs to the cluster the plugins scan.

`-types` and `-exclude-types` narrow plugin results and the results of your
own list blocks too.

```sh
tforphans -no-query \
  -state https://gitlab.example.com/api/v4/projects/15/terraform/state/vault \
  -plugin examples/plugins/vault.sh
```

`-no-query` still reads the states of any root directories given, but runs
no list queries; it needs at least one `-scan` or `-plugin`. [examples/plugins/vault.sh](examples/plugins/vault.sh)
covers Vault auth methods, roles, secret engines and policies with the
`vault` CLI and `jq`, and marks every object `unverified`: scan one Vault
per run with its own states and `-plugin-cluster-states`.

## In CI

A scheduled job that fails when something is unmanaged or could not be
checked, with the JSON report as an artifact (GitLab):

```yaml
tforphans:
  stage: audit
  rules:
    - if: $CI_PIPELINE_SOURCE == "schedule"
  script:
    - terraform -chdir=infra init -input=false
    - tforphans -config audits/aws.yaml -strict -format json > tforphans.json
  artifacts:
    when: always
    paths: [tforphans.json]
```

Exit status `2` means unmanaged objects, `1` an error or, with `-strict`,
incomplete coverage; `jq '.orphans'` and `jq '.summary.coverage[] |
select(.status != "complete")'` show what to look at. Run it on a schedule
or on the default branch, with read-only credentials: commands
(`-plugin`, `-terraform`) and state URLs come only from the job's command
line, never from a config file a merge request could change.

## Security

- tforphans only reads. It runs `terraform providers schema`, `state pull`
  (only for workspaces `workspace list` printed as list lines),
  `workspace list`, `query`, and `apply -refresh-only` on a temporary root
  that holds nothing but data sources; it never applies your
  configuration, and plans it only with `-refresh` (`plan -refresh-only
  -lock=false`, which writes no state; data sources of your configuration
  are read then, as in any plan). `TF_CLI_ARGS` and `TF_CLI_ARGS_<command>` are not passed
  on (they could redirect state), and the temporary root ignores
  `TF_DATA_DIR` and `TF_WORKSPACE`. Run it with read-only credentials (AWS `ReadOnlyAccess`,
  GCP Viewer, Azure Reader, a Cloudflare "Read all resources" token, a
  Proxmox PVEAuditor token): it needs nothing more.
- It sends nothing anywhere itself except `-state` URLs, which get
  `TF_HTTP_USERNAME`/`TF_HTTP_PASSWORD`, only over https and only on the
  original host (a redirect to another https host, such as a presigned
  storage URL, is followed without them and without a Referer; a redirect
  from https to http is refused). There is no
  telemetry. State contents are kept in memory and never printed.
- Files it writes: a temporary `zz_tforphans_<pid>.tfquery.hcl` (and
  `zz_tforphans_<pid>_override.tf` when a provider is declared only in child
  modules, `zz_tforphans_<pid>_version_override.tf` with
  `-ignore-required-version`)
  next to your configuration, removed after the run, and a temporary
  directory for data source scans, removed as well. A run killed with
  SIGKILL can leave them behind; the next run removes the files of a
  process that no longer exists and refuses to start while another run is
  still working in the same root. Adding `zz_tforphans_*` to `.gitignore`
  keeps them out of commits in any case.
- `-state-roots` and `-init` run terraform in every root module they find,
  with your environment. Point them only at repositories you would run
  terraform in anyway: a backend block decides where `TF_HTTP_*` and cloud
  credentials go, and `terraform init` downloads and runs providers.
- `-plugin` commands run with `sh -c` and your environment. Reports and
  errors name plugins by position (`plugin1`, `plugin2`), never by the
  command line, which may carry tokens. `-state` URLs are printed without
  user info and query; user info in a URL counts as credentials, so it is
  sent only over https.
- The ignore file is read from the working directory. In a repository you do
  not control, check it (or pass `-ignore /dev/null`), since it can hide
  findings.
- Names from APIs are printed without control characters, and import blocks
  only contain valid type names and quoted values.

## Limitations

- Coverage is what providers can list. Check `-v` for skipped types and the
  warnings for failed queries; a type that is not listed cannot be reported.
  A list query that returns as many results as its limit (`-limit` for
  generated blocks, the block's own `limit` for yours, 100 if unset) is
  reported as not fully listed, since there may be more; so is one of your
  blocks whose limit is an expression.
- Some providers list objects they create implicitly (default VPCs, managed
  roles, auto-created records). Put them in the ignore file, or use a
  preset (`-ignore-preset gcp-system`).
- An API that is not enabled in a GCP project fails its queries ("API not
  enabled"). Such listings stay in `failed_queries` and are counted in
  `summary.api_not_enabled`, but they do not make coverage incomplete: a
  disabled service shows and accepts no objects, and GCP refuses to disable
  one that has some unless forced. `-strict-api` counts them as gaps.
- The temporary files are written next to the configuration; there is no
  separate work directory, and a provider version cannot be overridden for
  the run (the root's installed providers are used).
- Terragrunt units are read only as states (`-state-roots DIR -terragrunt`);
  their modules are not queried. List their providers' objects from a plain
  root with the same provider configuration, or with `-scan`.
- Generated configuration can need manual fixes when the API does not return
  everything the resource requires. For example, Cloudflare does not return
  a Worker's code, so `cloudflare_workers_script` needs `content_file`, and
  `cloudflare_account_member` needs `roles` or `policies`.

## Related tools

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
against Terraform 1.15 and 1.16. A `v*` tag builds the release with
GoReleaser (`.goreleaser.yaml`) as a draft, with checksums, SBOMs and a
build provenance attestation, and pushes the Homebrew cask to
`5xxeu/homebrew-tap` (secret `HOMEBREW_TAP_TOKEN`: a fine-grained token
with contents write on that repository only). The cask points at the
release files, so publish the draft soon after.

The integration test uses a fake provider (things are files in a temporary
directory) that implements list resources and resource identity, and runs
the whole flow including the import round trip.

## License

MIT, see [LICENSE](LICENSE).
