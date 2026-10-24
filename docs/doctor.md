# Readiness diagnostics

For a first audit, [`tforphans init`](init.md) can discover roots and save
`.tforphans.yaml` before running doctor. Init itself reads local files only;
doctor performs the readiness checks described below.

Run `tforphans doctor` before an audit, with the same roots and scope flags:

```sh
tforphans doctor -state-roots ../infra -arg region=eu-north-1
tforphans doctor -format json -check-auth .
tforphans doctor -offline .
```

Doctor reads the Terraform version, installed provider schemas, root
configuration, provider lock files, initialization data (including
`TF_DATA_DIR`), config, ignore file and presets. It uses the audit's query
planner to describe selected types, required arguments, aliases, child-only
providers, duplicate local names and other skipped types. User list blocks
are counted but never executed. No resource listing, `terraform query`,
refresh-only apply or plugin execution takes place. It does not initialize
plain Terraform roots or take state locks.
With only explicit `-state` sources and/or plugins, no roots, state-root
searches, Terragrunt or scans, Terraform is not needed and its version check
is `skipped`; the Terraform executable need not be installed.
Before reading each root, including state-only and offline roots, it removes marked temporary files of dead tforphans processes, reports the count as `root.stale`, and stops checking that root if cleanup fails.

The `discovery` check fails when no generated queries, user list blocks,
confirmed scan blocks or plugins are selected. Generated query counts follow `-types`,
`-exclude-types` and the existing planner; user list blocks retain the audit's
behavior and are not filtered by these flags. `-no-query` leaves out both
generated queries and user list blocks. A failed check suggests a supported
`-scan` adapter for the root's provider, or a plugin or another root.

For each scan, `scan.<name>.plan` uses the audit's data source planner and
reports the block count and skipped types after filters. Schema inspection
uses an isolated temporary root, the pinned lock entry and `init -plugin-dir`
pointing to that root's installed providers, without downloading providers.
It performs no plan, apply or cloud reads. This also works offline when the
provider is installed. An unavailable schema leaves the scan unconfirmed;
a scan with no confirmed listing blocks does not count towards discovery.
Only non-hidden blocks with selected resource listings count; context blocks
and parent blocks without their own listings do not. The plan check's
`count` is the number of these listing blocks, while `blocks` is the total
number of data source blocks. `-scan-scope state` uses the same state index
and parent IDs as the audit, preserving explicit scope overrides. Offline,
a plan needing state scope is `skipped` and does not confirm discovery.
A successfully built scan plan is `ok`. Provider and adapter limitations
(such as missing result identities, unsupported parent arguments or
pagination) are warnings when they leave coverage incomplete; deliberate
adapter exclusions are `skipped`. They do not require configuration fixes
or prevent readiness. A missing Terraform data directory is a warning;
operations that actually fail because initialization is needed still fail.
A state-only root without Terraform configuration also receives a warning;
state reads determine its readiness. A query root still needs configuration.
Schema errors and zero discovery blocks still fail their respective checks.

Query and scan plan skips are grouped by category and status for each root.
Each group shows the number of types and the first eight names; `-v` shows
all names in text. JSON always includes the full `types` and `skipped_types`
with the original reasons. Query skips remain failures when arguments or
provider configuration can fix them; a user list block for the same type and
provider configuration covers a missing generated argument without an
`-arg` suggestion; explicit `-no-aliases` is a warning.

By default it lists workspaces and pulls their stored states, plus `-state`
sources and roots under `-state-roots`. State reads use `-max-state-size`,
`-state-parallelism`, `-source-timeout` and `-timeout`. Independent checks
continue after failures. Counts of instances without resource identity apply
to list-capable types; matching by attributes still works, so this is a
warning. A successful state pull proves access to the backend, not permission
to read cloud resources.

`-check-auth` is opt-in and accepted only on the command line, never from
a config file (including `-config`). A targeted identity plan can evaluate
provider configuration dependencies. `offline` may be set in shared config;
the audit and adopt commands ignore that key.
Provider configurations used by user list blocks are included in identity
checks even with `-no-aliases`; each configuration is checked only once.
AWS, Google and Azure use the existing identity data sources with targeted
plans, refresh disabled and locking disabled. Proxmox
uses only its adapter probe in a temporary root with the pinned provider and
environment credentials, also through a targeted plan. Neither runs a full
scan. Other providers, including Cloudflare, report authentication as
`skipped` with a reason. Identity checks can show account, project,
subscription and location IDs. They do not prove resource read permissions
or guarantee that a later scan succeeds.

`-offline` skips workspace enumeration, all state reads (including local
`-state` files), and authentication. The result is always `unknown`. Version
and installed schemas can still be inspected; uninitialized roots do not run
schema inspection offline. Provider schema inspection starts installed
provider executables, so this option alone cannot enforce network isolation
against a provider or wrapper that contacts the network during startup.

Configured plugins have a `skipped` check because their protocol has no diagnostic
mode. Plugin commands are never started, including with `-check-auth`.

## Flags and config

For units, as during an audit, Terragrunt may initialize the unit and write
`.terragrunt-cache`.

Doctor accepts positional `ROOT_DIR...` and these audit flags:

- `-config`, `-no-config`, `-terraform`
- `-state`, `-state-roots`, `-current-workspace`
- `-terragrunt`, `-terragrunt-unit`
- `-types`, `-exclude-types`, `-arg`, `-no-aliases`
- `-scan`, `-scan-scope`, `-no-query`, `-plugin`
- `-format text|json`, `-v`, `-quiet`
- `-timeout`, `-source-timeout`, `-max-state-size`, `-state-parallelism`, `-max-roots`

Its own flags are `-offline` and `-check-auth`. Other command-line flags,
including `-init`, `-refresh`, `-allow-unpinned`, `-baseline`, and adopt
options, are rejected. `-no-query` retains the audit's requirement for a scan
or plugin. The same flag value parsers and config security rules apply.
Unrelated recognized config keys are ignored, so one config file can serve
both commands. Ignore file and preset options in config are loaded for
validation even though doctor does not filter resource findings.

## Output and exit status

Text output starts with the verdict: `READY` (status `ready`), `BLOCKED`
(`needs_configuration`) or `UNKNOWN` (`unknown`), with the number of
problems, warnings, passed and skipped checks. Then come `Problems`
(failed checks) and `Warnings`, problems in the order they are fixed: config
and Terraform first, then roots, states, provider schemas and listings, and
discovery last, because it depends on the others. A check with the same
outcome in several roots is printed once with the list of roots, and a root
in its fix becomes `<root>`. Passed and skipped checks are listed only with
`-v`, which prints every check in the order it ran. The last block, `Next`,
proposes the audit command when nothing failed, and otherwise the same
command as `tforphans doctor ...` to check again after the fixes: an audit
started with failed checks would fail or miss what they hide. JSON
`next_command` is always the audit command; run it only when `status` is
`ready`.

A root with no listings selected (no list resources, no user list blocks) is
a `query.plan` warning: the audit only reads its state. Without `ROOT_DIR`,
the current directory is a root only when it has Terraform configuration or
no other state source is given (`-state`, `-state-roots`,
`-terragrunt-unit`); otherwise only those states are read and a note says
so. When no listing is selected and a root uses a provider with a `-scan`
adapter, the `discovery` failure suggests that adapter, also for roots used
only for their state.

Only diagnostic results go to stdout. Progress goes to stderr on a terminal,
unless `-quiet` is set. Text includes a proposed next audit command, with shell
quoting. JSON contains the command as an argument array. Doctor-only flags
`-offline`, `-check-auth` and `-format` are removed only in flag positions;
string values such as `-state -offline` are preserved. The command uses the
same parsed arguments as doctor, including flags after roots. The proposed
command puts all flags before `--` and positional roots after it, preserving
roots named like subcommands or starting with a hyphen. Missing argument
suggestions are inserted before `--`, when present.
When a suggestion adds the first CLI occurrence of a repeated flag, effective
config values for that flag are copied into the command too, with the same
redaction as CLI values. This preserves config arguments and scans instead
of replacing their lists with the fix. Existing CLI overrides remain in effect.
Argument suggestions are collected across all roots:
when every type needing an argument has the same suggested value, the flag
is global (`-arg NAME=VALUE`). Different suggested values use one
`-arg TYPE:NAME=VALUE` per type, including in each check's remediation.
State URLs use the same credential redaction as audit reports. URL lists in
`-arg` are redacted value by value; userinfo is also removed from every
embedded URL in all next-command arguments. Redaction applies only to URLs;
globs and paths containing `?` are preserved literally. URL queries, including
commas and quotes, are redacted in full. Plugin commands and potentially
sensitive argument values are replaced with placeholders. Review and fill placeholders before running the
command; redacted URLs may also need their original credentials restored.
User `-arg` values are preserved unless the argument name contains token,
secret, password or credential, or ends in a separate key component (for
example `key`, `api_key`, `private-key` or camel-case `apiKey`). Names such as `key_vault_name`,
`key_name` and `keyboard` are preserved. Only arguments needing shell quoting
are quoted in text output. Remediation and command text are cleaned before
printing. Next-command arguments with control or Unicode format characters
are replaced with placeholders in both formats, with an explanatory note.
JSON otherwise retains exact remediation and command values, escaping
control characters during encoding.

A state-only root has one `root.query` skip instead of individual query and
authentication skips. When any states were read, the result includes one
backend access explanation, in the JSON `notes` array or after the text
result. This explanation does not count as a warning.

| Exit | Meaning |
|:-----|:--------|
| 0 | Ready in the selected scope, warnings are allowed. Offline: no failed local checks. |
| 2 | Completed, with configuration or access obstacles. Offline: failed local checks. |
| 1 | Could not complete diagnostics: invalid flags/config, malformed Terraform replies, cancellation or overall timeout. |

Readiness means that the audit can be attempted in its selected scope. It
means neither that there are no orphans nor that a future scan will succeed.
Offline status is `unknown`, even with exit 0 or 2.

## JSON schema, version 1

`-format json` writes exactly one JSON document:

```json
{"schema_version":1,"status":"ready","checks":[{"id":"terraform.version","status":"ok","message":"1.16.4"}],"next_command":["tforphans","."]}
```

- `schema_version`: integer, currently 1, independently versioned from audit JSON.
- `status`: `ready`, `needs_configuration` or `unknown`.
- `checks`: ordered array of check objects. An ID may occur several times,
  for different roots, providers, skipped types or states.
- `next_command`: array of shell arguments, beginning with `tforphans`.
- `notes`: optional array of explanations, including the distinction between
  backend access and cloud read permissions when states were read.

Every check has `id`, `status` (`ok`, `warning`, `failed`, `skipped`) and
`message`. Optional fields are `remediation` (a fix command or instructions),
`root`, `provider` (source address), `category` (plan skip category), `types` (list-capable types), `read` and
`total` (state counts), `blocks` (total scan data source blocks), `count` (removed stale files, instances without identity, or selected discovery operations), and
`skipped_types` (the existing query planner's `type`, `reason`, optional
`root`, `provider`, `excluded`). State totals include all enumerated
workspaces and explicit sources; a root whose workspace enumeration failed
counts as one unread source because its workspace total is unknown.

Stable check IDs:

| ID | Checks |
|:---|:-------|
| `config` | Flag and config parsing |
| `offline` | Network isolation limitation of schema startup |
| `ignore` | Ignore file and preset parsing |
| `roots` | Root discovery, limits and shared data directory safety |
| `terraform.version` | Terraform version and, when selected, query support |
| `discovery` | Selected query, confirmed scan block and plugin count; fails when zero |
| `root.stale` | Stale temporary file cleanup and failures |
| `root.config` | Root configuration availability and parsing |
| `root.initialized` | Terraform data directory availability |
| `root.lock` | Local provider lock file syntax and availability |
| `root.query` | Query checks skipped for a state-only root |
| `providers.schema` | Installed provider schema and available list types |
| `query.plan` | Generated and user listings, planning notes and skips |
| `query.arguments` | Required list arguments |
| `query.aliases` | Aliases and provider configuration gaps |
| `states.read` | State and workspace availability, read counts |
| `states.identity` | List-capable instances without resource identity |
| `scan.<name>.plan` | Data source block count, planning skips and schema availability |
| `scan.<name>.pin` | A version and hashes for the scan provider |
| `scan.scope` | State-derived scan scope has readable states |
| `auth.<ref>` | Identity of a local provider reference, including aliases |
| `auth.<name>` | Authentication probe for a scan adapter |
| `auth.providers` | Reason no provider identity checks could run |
| `plugins` | Plugin diagnostics skipped |
| `run` | Cancellation or overall deadline |

Checks that cannot run include a `skipped` reason. Invalid CLI/config input
exits before a report is created and writes the parsing error to stderr.
State errors use the same shortened diagnostic as the audit's `unread_states`,
with state source URLs redacted. Known initialization, backend access, state
size and source timeout failures include a specific remediation. Malformed
Terraform or state replies are identified by error type, not message text.

Plan skips marked `excluded` do not fail diagnostics (including opt-in types
and duplicate listings through equivalent provider configurations). Other
plan gaps fail diagnostics. Explicit `-no-aliases` skips are warnings in
doctor because they are the user's choice; the audit counts them as coverage
gaps. No list queries run, so doctor cannot confirm whether a user list block
will execute successfully or cover a generated plan skip.

Skipped-type details also carry audit [reason codes](reasons.md):
`reason_code`, optional `action`, and optional `missing_keys` (names only).
