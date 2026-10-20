# Plugins

A plugin is any command that prints one JSON object per line:

```json
{"type": "vault_policy", "identity": {"id": "ci"}, "display_name": "ci", "import_id": "ci"}
{"error": "sys/auth: permission denied"}
{"done": true}
```

Output is parsed while the command runs. Each run may return at most
1,000,000 resources and 1 GiB of stdout, with a 64 MiB limit per line.
Warnings and blank lines count toward the byte limit. Exceeding a limit
fails the plugin and kills its process group while the command is still
running. After the command exits, tforphans stops reading its output;
descendants that outlived it may keep running. Errors name the plugin by
position (`plugin1`, `plugin2`), never its command line. A key repeated
in one object, also in another case (`"type"` and `"Type"`), fails the
plugin: JSON readers disagree on which copy wins.

- `type`: Terraform resource type.
- `identity`: attributes to match. Against state without resource identity
  they are compared with the instance's attributes, so `{"id": ...}` with the
  value the provider stores as `id` is usually enough. All keys must match.
  A number Terraform cannot read (such as `1e7000000000`) fails the plugin,
  and so does a `null` value (`jq` gives one for a missing field): the other
  keys, such as an account, alone would match.
- `import_id`: optional; import blocks then use `id = ...`.
- `provider`: optional provider configuration for the import block
  (`vault.other`); `provider_source`: optional provider address
  (`registry.terraform.io/hashicorp/vault`), so that state of another
  provider with a type of the same name does not match. A `type` or
  `provider` that is not a valid name makes the plugin fail.
- `error` lines are reported as warnings (errors with `-strict`).
- `{"done": true}` confirms that the check finished, including when it found
  no objects. This object must contain only `done`, with the value `true`.
  It may appear anywhere in the output and may be repeated.
- `unverified`: optional, `true` when state cannot confirm where the object
  lives (Vault state does not record the Vault address, so a policy `ci`
  in state of another Vault would match). Such objects match only as
  uncertain (an error with `-strict`) unless `-plugin-cluster-states`
  states that every state given belongs to the cluster the plugins scan.

A command that exits successfully without resources, `error` lines or
`done` leaves a coverage gap: `summary.failed_sources` records `pluginN`,
`complete` and `discovery_complete` are false, and the run exits 1 with or
without `-strict`. Blank lines do not count as output. Resources
without `done` remain valid, and `error` lines without `done` remain
warnings. This check uses the raw output, before type filtering.

`-types` and `-exclude-types` narrow plugin results and the results of your
own list blocks too.

```sh
tforphans -no-query \
  -state https://gitlab.example.com/api/v4/projects/15/terraform/state/vault \
  -plugin examples/plugins/vault.sh
```

`-no-query` still reads the states of any root directories given, but runs
no list queries; it needs at least one `-scan` or `-plugin`. [examples/plugins/vault.sh](../examples/plugins/vault.sh)
covers Vault auth methods, roles, secret engines and policies with the
`vault` CLI and `jq`, and marks every object `unverified`: scan one Vault
per run with its own states and `-plugin-cluster-states`.

## Junos

[examples/plugins/junos.py](../examples/plugins/junos.py) (Python 3, no other
packages) covers `jeremmfr/junos` devices, whose provider has no list
resources: it reads the configuration as JSON (`show configuration |
display json`) and lists configured physical and logical interfaces, BGP
groups and neighbors (also in routing instances), policy statements, prefix
lists, security zones, applications and application sets, with the import
IDs the provider takes (`transit_-_default` for a BGP group).

```sh
# over ssh (keys from your agent or ~/.ssh/config), one device per run
tforphans -no-query -state-roots junos/fw1 \
  -plugin 'python3 examples/plugins/junos.py --ssh admin@fw1' -plugin-cluster-states
# or from a saved configuration: it holds secrets, so use a new private
# file (mktemp creates it with mode 0600) and remove it on exit
# (the plugin command runs through sh -c, so pass the path in the environment)
export cfg=$(mktemp) && trap 'rm -f "$cfg"' EXIT
ssh admin@fw1 'show configuration | display json | no-more' > "$cfg"
tforphans -no-query -state-roots junos/fw1 -plugin 'python3 examples/plugins/junos.py "$cfg"'
```

Junos state does not record the device, so every object is `unverified`:
pass `-plugin-cluster-states` only when the states given are those of that
one device. Only names leave the script; the configuration (with its `$9$`
secrets and password hashes) is never printed. A physical interface whose
stanza holds only units counts as configured through its logical
interfaces, and objects that come from groups (`apply-groups`) are not
listed. A device that cannot be read is a warning (coverage incomplete),
not a failed run. The plugin is new: check its results against your own
device before relying on them.

[Back to the README](../README.md) · [All guides](README.md)
