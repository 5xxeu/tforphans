# Roots, Modules and States

## Resources in modules

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

## Several roots and states

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
are read in parallel; `-state-parallelism N` (1 to 64) changes that, for
example to stay under a backend's request rate limit. A root that is not initialized, or whose state cannot
be read, is reported as a warning (an error with `-strict`), because its
resources may then show up as unmanaged; the last line names each unread
root with the line of the error that says why, and so does
`summary.unread_states` (`[{"root": ..., "error": ...}]`). A workspace that
fails does not drop the others: their states still count, and the warning
names the workspace. Directories that cannot be read are skipped with a
warning, and so is a directory without a backend found where a `.tf` or
`.tf.json` file cannot be read or parsed (a broken symlink, a stray `}`):
it may be a root, so it counts as an unread state. More than `-max-roots` root modules (200 by default) stop the run
before terraform starts in any of them: a wrong path (`..` relative to a
config file somewhere else) can otherwise reach a whole home directory. `-init` runs `terraform init` in
uninitialized roots first; it creates `.terraform` there and may write a
lock file, so it is opt-in. A shared `TF_PLUGIN_CACHE_DIR` makes it fast; terraform does not promise
that the cache is safe for parallel installs, so tforphans runs one `init`
at a time, also while it reads states in parallel.
Terraform keeps a root's backend settings in its data directory: with a
partial backend configuration (`backend "s3" {}` and `-backend-config` at
init) a root that shares it with another would silently read the other's
state. When two roots end up in one data directory (an absolute
`TF_DATA_DIR`, or a relative one such as `../shared` for sibling roots),
the run stops before terraform starts; a path inside each root
(`.terraform-x`) works.
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

## Terragrunt

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
terragrunt command does. Units are read in parallel, but their inits run one
at a time, together with the inits of `-init` (a shared plugin cache is not
safe for concurrent installs): tforphans gives itself to terragrunt as its
terraform (`TG_TF_PATH`), runs the terraform of `-terraform` for it, and
waits for the turn before `init`. Time spent waiting for that turn does not
count against `-source-timeout`. Terragrunt
also evaluates the units' HCL, which can run commands (`run_cmd`, hooks):
the flag is opt-in and accepted only on the command line, like `-plugin`. It runs the terraform of
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

[Back to the README](../README.md) · [All guides](README.md)
