# Config File

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

[Back to the README](../README.md) · [All guides](README.md)
