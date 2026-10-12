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
unknown too. These matches are counted and reported as unverified (an error
with `-strict`), and a note says how many state instances have no identity;
`terraform apply -refresh-only` with a current provider records identities
in state and makes them verifiable (it writes the state, so it needs write
access). The boot disk of a managed `google_compute_instance` counts as
managed by it; disks attached with `attached_disk` do not, since they may
be created elsewhere. Whatever matches nothing is reported.

## Install

```sh
go install github.com/5xxeu/tforphans@latest
```

Requires Terraform 1.14 or newer in `PATH` (or `-terraform /path/to/bin`).
OpenTofu has no `query` command yet; with it only `-no-query` mode works.

## Usage

```sh
cd infra/aws/prod && terraform init
tforphans                     # same as: tforphans .
```

```
TYPE           NAME             IDENTITY                                                SOURCE
aws_instance   debug-box        account_id=123456789012, id=i-0abc, region=eu-north-1   .
aws_s3_bucket  tmp-export-2024  account_id=123456789012, bucket=tmp-export-2024, ...    .

512 live, 498 managed, 12 ignored, 2 unmanaged; 187 resource types queried, 58 skipped (-v to list)
```

(Illustrative output.)

Exit status: `0` nothing found, `2` unmanaged resources found, `1` error.
A run that queried no resource type at all is an error too, since it could
not have found anything (for example a provider version without list
resources; the error names such providers). With `-strict`, incomplete
coverage (a failed list query or a plugin error) is also an error, which
suits CI.

Queries that did not list everything are summed up by cause: missing
argument, permission denied, truncated, not found, API not enabled, other.
Messages are cut to one line; `-v` prints each failed query.

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
resources may then show up as unmanaged. A workspace that fails does not
drop the others: their states still count, and the warning names the
workspace. `-init` runs `terraform init` in
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

### Regions, locations and other list arguments

List queries use the provider configuration of the root module, so by
default AWS is listed in the provider's region only. `-arg` fans every list
query whose config has that string argument out over several values:

```sh
tforphans -arg region=eu-north-1,eu-central-1,us-east-1
tforphans -arg location=global,europe-north1   # google
```

An argument can be limited to some types with `TYPE_GLOB:`, and then wins
over the same argument without one for those types. Google Compute
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
apply).

For anything more specific (provider aliases, other accounts, filters), put
your own `list` blocks in a `*.tfquery.hcl` file in the root module. They run
together with the generated ones and their results are checked the same way.

Generated blocks always use the provider's default configuration. A root
that configures a provider only with aliases (`provider "aws" { alias =
"prod" }` and no block without an alias) gets no generated blocks for it,
since the empty default configuration could reach another account through
the environment; a note says so, and your own list blocks with `provider =
aws.prod` cover it. A provider with no provider block at all is queried
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
that rule's types (`include_resource`, slower):

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
Cloud and GKE create on their own: GKE nodes, their disks and persistent
volume disks (by GKE labels and `gke-`/`pvc-` names), GKE and load balancer
firewall rules and addresses, default service accounts, and App Engine,
Cloud Functions and Cloud Build buckets. See
[the preset](internal/report/presets/gcp-system.ignore) and check what it
hides before relying on it.

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

`adopt` exits 0 whether or not it found anything (1 on errors, and with
`-strict` on incomplete coverage); the plain command is the one for CI
checks.

`-format json` prints the orphans together with the coverage summary: counts
per type (`summary.by_type`), skipped types, failed queries with their
`category`. Every orphan has the same keys. `-include-attrs
labels,creation_timestamp` adds those attributes to each orphan (to see how
old an object is, or who owns it). `-format summary` prints only a table of
counts per type, types with unmanaged objects first.

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
still listed. With
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
repeat across clusters too. So all Proxmox matches are unverified (an error
with `-strict`) unless you state that every state given belongs to the
scanned cluster:

```sh
tforphans -no-query -scan proxmox -state-roots ~/work/proxmox/cluster-a \
  -proxmox-cluster-states -strict
```

Scan one cluster per run, with that cluster's states only.

For Cloudflare, state is matched by `id` plus `zone_id` or `account_id`. Import IDs follow
the provider's convention (`<zone_id>/<id>`, or `accounts/<account_id>/<id>`
for resources that can live in an account or a zone). On a small account
the scan lists about 95 of the 271 resource types in under a minute; most of
the rest are settings and singletons, which cannot be orphaned (`-v` lists
every skipped type with the reason).

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
  unverified (an error with `-strict`) unless `-plugin-cluster-states`
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

## Security

- tforphans only reads. It runs `terraform providers schema`, `state pull`
  (only for workspaces `workspace list` printed as list lines),
  `workspace list`, `query`, and `apply -refresh-only` on a temporary root
  that holds nothing but data sources; it never plans or applies your
  configuration. `TF_CLI_ARGS` and `TF_CLI_ARGS_<command>` are not passed
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
  SIGKILL can leave them behind.
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
  enabled"); this still counts as incomplete coverage for `-strict`, since
  objects created before the API was disabled cannot be ruled out. Exclude
  the types of APIs you never use with `-exclude-types`.
- The temporary files are written next to the configuration; there is no
  separate work directory, and a provider version cannot be overridden for
  the run (the root's installed providers are used).
- Terragrunt is not supported yet: generated backends live in
  `.terragrunt-cache`, which `-state-roots` skips. Pass such states with
  `-state` or run tforphans in the generated directories.
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

The integration test uses a fake provider (things are files in a temporary
directory) that implements list resources and resource identity, and runs
the whole flow including the import round trip.

## License

MIT, see [LICENSE](LICENSE).
