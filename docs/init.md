# Initialize an audit

Run `tforphans init` in your infrastructure repository to prepare a reusable
configuration:

```sh
cd infrastructure
tforphans init
tforphans doctor
tforphans
```

Init reads local Terraform configuration and `.terraform.lock.hcl`, shows
roots, provider sources, locked versions, aliases and backend types, then
creates `.tforphans.yaml` in the current directory:

```yaml
roots:
  - stacks/network
  - stacks/production
state-roots:
  - .
```

The paths select query roots and state search directories for subsequent
runs. State search includes new roots under those directories too; newly
added roots become query roots after `init -update`. Roots already selected
for queries have their states read only once. State-only directories are
included through `state-roots`, without trying to query them.

Init works without Terraform installed. It does not start Terraform,
providers or plugins, pull states, fetch modules, check credentials or list
cloud resources. It reports a missing Terraform data directory and suggests
`terraform -chdir=DIR init`; `TF_DATA_DIR` is respected when checking for that
directory. Locked versions are shown as metadata, without checking installed
provider binaries or hashes. Providers present only in the lock file may
belong to child modules. Unlocked providers are labeled `not locked`.

Run `tforphans doctor` next to check installed provider listing capabilities,
missing arguments, backend access and other prerequisites. A prepared config
does not mean access or cloud coverage is confirmed. Credentials remain in
the environment or Terraform configuration; their values and backend
addresses are not copied into the generated file or printed by init.

## Select directories

```sh
tforphans init teams/platform teams/product
tforphans init -dry-run stacks/production
tforphans init -- -directory-starting-with-dash
```

Directories must exist and resolve inside the current directory, as required
for automatically loaded configs. Init walks the selected trees and recognizes
nested roots by a backend or cloud block, `terraform.tfstate`, or
`terraform.tfstate.d`. Hidden directories, `vendor` and `testdata` are skipped;
directory symlinks are not followed. A named directory with Terraform
configuration is also a root even before its first init or state exists.
For a new local root without a backend or state, pass its directory explicitly.
Child modules without root markers are not selected just because they hold
`.tf` files. Terragrunt execution and unit selection remain explicit CLI
options; this command does not infer them.

The default limit is 200 discovered roots, including state-only roots.
`-max-roots N` changes it; `0` removes the limit. A limit given on the command
line is saved as `max-roots`, which the audit applies to `-state-roots`, and an
update without the flag keeps the saved one. An unreadable or malformed
configuration that prevents root discovery causes exit `1` and no config
write. No configuration found is also an error. Only the selected directory
trees are searched; init cannot infer states in other checkouts or services.
Add other state sources before trusting findings across shared accounts.

## Scan adapters

Init reports when a discovered provider has a Cloudflare or Proxmox adapter
that the config does not enable, recommends it before the first audit
(without it the audit reads the provider's states but lists none of its
objects) and prints the `init -update -scan ...` command that enables it;
`Next:` then names that command before `tforphans doctor`.
These adapters use credentials from the environment, which may target another
account or cluster than a root's provider block. They are enabled only when
explicitly selected:

```sh
tforphans init -scan cloudflare
# For a config that already exists:
tforphans init -update -scan cloudflare
```

This writes `scan: [cloudflare]` for subsequent doctor/audit runs; init itself
does not scan. Repeat `-scan` to select several adapters. Supplying `-scan`
replaces the existing scan list; omitting it during `-update` preserves that
list. Plugins are not auto-selected or saved: plugin commands remain CLI-only.
Account, project, region and zone discovery is not part of this command.

## Preview and update

```sh
tforphans init -dry-run
tforphans init -update -dry-run
tforphans init -update
tforphans init -update teams/platform
```

An existing `.tforphans.yaml` is preserved unless `-update` is given.
Without new directory arguments, an update reuses the existing `state-roots`,
falling back to `roots`, then `.` if neither is set. Explicit directory
arguments replace this selection. Updating replaces `roots` and `state-roots`
with the discovered query roots and chosen search trees; previous roots not
found again are listed as `Removed from roots`, and search trees that explicit
directory arguments leave out as `Removed from state-roots` (pass them as DIR
to keep them). A saved `max-roots` is read as the audit reads it, so `"5"`
counts as 5.
All other keys and
comments are preserved, including a file of comments only; YAML formatting may
change. YAML aliases are expanded
before editing so that other settings keep their previous values. Existing filters such as
`types` and `exclude-types` continue to apply. Init does not validate every
saved audit flag; doctor checks the resulting configuration.

`-dry-run` prints discovery and the proposed file without writing it. An
existing file still needs `-update -dry-run` to preview its update. Config
symlinks and non-regular files are refused for updates. The replacement is
prepared beside the config, retains its permissions, and is renamed into
place only after checking the original has not changed during discovery.
The check and the rename run under `.tforphans.yaml.lock`, so that two
updates at the same time cannot both pass the check; the second one fails
and can be repeated. A lock left by a killed init is not removed
automatically: delete it when no init is running.

Output is grouped into sections (`Terraform roots`, `Scan adapters`, `Not
checked`, the proposed file or `Result`) and colored in a terminal under the
same rules as the audit report (see [output](usage.md)). The saved file
contains no color codes; the `-dry-run` preview spells out control and format
characters from directory names and comments (`\u202E`).

Exit status is `0` when the file or preview is prepared, `1` on invalid input,
incomplete discovery or a write error. `-h` shows help and exits `0`.

`init` as the first argument is the command: audit a root directory named
`init` with `tforphans ./init` or `tforphans -- init`.

[Back to the README](../README.md) · [All guides](README.md)
