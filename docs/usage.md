# Output, Exit Status and Coverage

```sh
cd infra/aws/prod && terraform init
tforphans                     # same as: tforphans .
```

```
== Summary =================================================
512 live, 498 managed, 0 uncertain, 12 ignored, 2 unmanaged
1 states read, 187 resource types queried, 58 skipped (-v to list)
managed objects matched by identity: 470, by attributes: 28

== Coverage ================================================
coverage: complete

== Unmanaged resources (2) =================================
All found by terraform query in ..
  TYPE           NAME             IDENTITY
  aws_instance   debug-box        account_id=123456789012, id=i-0abc, region=eu-north-1

  aws_s3_bucket  tmp-export-2024  account_id=123456789012, bucket=tmp-export-2024, ...
```

(Illustrative output.)

Human-readable output separates totals, diagnostics, coverage and findings
into labeled blocks. The summary (on stderr) comes first, so that a long
table does not push the totals and the coverage verdict off the screen;
`Baseline` (with `-baseline`) and `Result scopes` (with `-v`) come before
`Coverage`. The findings follow on stdout, grouped by scope (account,
zone, project) with one heading per scope; identity keys that the heading
names (`account_id`, `zone_id`, `project`, `subscription_id`) are left out
of its rows, and a source shared by all findings is named once above the
table instead of in a column. With `-format json`, `-format summary` and
`adopt` the summary stays at the end of stderr. A block with nothing
to report has no heading. In a terminal, cyan headings mark sections, green marks
managed counts and complete coverage, yellow marks findings, uncertainty,
warnings and coverage with exceptions, and red marks errors. Labels remain
visible without color. Colors are disabled on Windows, for redirected
streams, `TERM=dumb`, or a nonempty `NO_COLOR` environment variable
(`NO_COLOR=1 tforphans ...`). JSON, generated queries and import blocks
contain no presentation codes.

`-max-state-size` limits each state read to 1 GiB by default, including
`-state` files, stdin and HTTP responses, Terraform and Terragrunt state
pulls, and JSON output from `-refresh`. Oversized input is rejected with an
error that names the limit and suggests raising it. Give a positive byte
count or an integer with a `K`, `M` or `G` suffix (powers of 1024,
case-insensitive), for example `-max-state-size 512M` or
`-max-state-size 2G`. Zero and negative sizes are invalid. Automatic
`.tforphans.yaml` files may set `max-state-size` only up to the 1 GiB default;
a larger value is an error even when overridden on the command line. To
raise the limit, pass it on the command line or via `-config FILE`.

`terraform query` output is read as it comes, and attributes not asked for
are dropped from each object right away. `-max-query-size` (1 GiB by
default, same rules for config files) bounds the output of one query, and
one query may return at most 1,000,000 objects. `-timeout 30m` stops the
whole run; `-source-timeout 10m` gives up on one root, `-scan` or plugin
and goes on with the others (waiting for the turn to run `terraform init`,
which runs one root at a time, does not count).

When one root, `-scan` or plugin fails as a whole (a query over the limit,
a plugin that exits with an error or runs out of time), the run goes on with
the others and prints its report with the failure in
`summary.failed_sources` (and as a warning); coverage is incomplete and the
exit status is `1`. A root that failed before its state was read is also an
unread state, so `adopt` refuses to write import blocks unless
`-allow-incomplete` is given. Cancelling with Ctrl-C still stops at once.

Exit status: `0` nothing found, `2` unmanaged resources found, `1` error.
Without `-strict` the status speaks of findings only: a run with incomplete
coverage and nothing found exits `0`, so check `coverage:` (or
`summary.complete`) or pass `-strict`. A run that queried no resource type
at all is an error too, since it could not have found anything: every
listing failed (the error counts them by cause; usually the credentials),
or a provider version has no list resources (the error names such
providers). With `-strict`, incomplete
coverage (a failed list query, a plugin error, a type skipped for want of
an argument) is also an error, which suits CI. A root given as an argument
that is not initialized and fails for it is not queried and its state not
read: a warning names it, coverage is incomplete, and the other roots still
run (`-init` runs `terraform init` there first).

Duplicate state ownership is also checked across every state read. A warning
lists confirmed ownership by different states, possible matches (with their
reason), and objects held at several addresses within one state. It shows
the first eight objects; `-format json` includes all owners and sources.
These warnings stay visible with `-quiet`, like other warnings, and do not
fail `-strict` or change the default exit status. `-strict-ownership` exits
`1` for confirmed duplicate ownership or any duplicate within a state;
possible ownership alone never fails it. The flag only makes a run stricter,
so automatic `.tforphans.yaml` files may set `strict-ownership: true`.
Import blocks from `adopt` are unchanged.

Queries that did not list everything are summed up by cause: crashed,
missing argument, permission denied, truncated, not found, request rejected
(often a feature the plan does not include), API retired, provider bug, API
not enabled, other. Messages are cut to one line; `-v` prints each failed
query. A cause with many types names the first eight (a token without most
permissions would otherwise bury a crash in a list of 77).

Every live object ends up in one of four groups: **managed** (matched to
state, its account or cluster confirmed), **uncertain** (matched to state,
but state cannot say whether the instance is in the same account or
cluster; see below), **ignored** and **unmanaged**. Uncertain objects are
not counted as managed and get no import block; the text output lists them
after the unmanaged ones with the state address they matched. The summary
also says how the managed objects were matched: by the resource identity in
state (exact) or by attributes.

`-explain` says for every live object why it landed in its group, after the
text report or as `explain` in JSON (not with `adopt` or `-format summary`):

```
STATUS     TYPE          NAME    IDENTITY   WHY
managed    vault_policy  kept    id=kept    vault_policy.kept in infra (default), by resource identity
uncertain  aws_instance  web     id=i-0abc  matches aws_instance.web in infra (default), by attributes (the state instance has no resource identity), but the state does not hold account_id to confirm where the object lives
ignored    vault_policy  tmp-1   id=tmp-1   ignore rule .tforphans-ignore:3: vault_policy:tmp-*
unmanaged  vault_mount   kv      path=kv    closest: vault_mount.kv in infra (default): its identity has no namespace
```

A managed or uncertain object names the state address and where that state
was read; an ignored one the file and line or preset of the rule; an
unmanaged one up to three state instances that share a key value and what
differs (provider, a key's value, a key the state does not hold), or that
no state instance of the type was read. Use it with `-types` to look at one
type, for example when an object you manage shows up as unmanaged.

The `coverage:` line says whether "nothing unmanaged" can be trusted: `coverage:
complete`, or `coverage: INCOMPLETE` with what was not checked (failed or
truncated listings, unread states, plugin errors, and types that needed a
check and were skipped in a root: a list resource with a required argument
not given with `-arg`, a provider configuration left out with `-no-aliases`). A type
left out on purpose (`-exclude-types`, opt-in types, google-beta listed
through google, a scope not asked for) or one with no list resource or
data source at all does not make coverage incomplete; the line then reads
`coverage: complete for what can be listed; not checked: ...`, and `-v`
lists those types. A type listed in one root does not cover a root where
it was skipped, since the roots may look at different accounts.
`-strict-ignore TYPE_GLOB,...` names types whose failed listings and
missing arguments do not make coverage incomplete either: they are still
listed where possible and still reported (`summary.strict_ignored`, and
coverage rows with the reason `-strict-ignore: ...`), so that a CI gate
fails only on what it did not expect, for example a product the project
does not use (`-strict-ignore 'google_firebase_*'`). Unlike
`-exclude-types`, which does not list a type at all, its objects are still
found. Before the slow part,
a `plan:` line says what the run checks: which roots are queried, how many
states are read, what is scanned and with which scope. On a terminal,
progress lines follow (each root's state read, each root queried, each
`-scan`), so that a long run does not look hung; `-quiet` turns them off,
and they are not printed when stderr is not a terminal (CI logs).

## Baselines in CI

Commit an initial JSON report, then fail only on new findings:

```sh
tforphans -format json > baseline.json
git add baseline.json
# In CI, using the same roots, states and discovery options:
tforphans -baseline baseline.json -strict
```

With `-baseline FILE`, exit `2` means new orphans exist. Known orphans stay
in JSON, marked `"baseline": "known"`, and are hidden in text output.
A line in the `Baseline` block, before `Coverage`, counts new, known,
resolved and not verified findings (`-v` lists the not verified ones).
`-strict` still exits `1` for incomplete coverage or uncertain matches,
even when every orphan is known. `adopt` does not accept `-baseline`.
An automatic `.tforphans.yaml` may set `baseline: baseline.json`, with the
same path restrictions as `ignore`.

A previous finding that is absent now is resolved only when the same listing,
unchanged as written, fully ran again and no longer returned the object.
The listing's source, address including its instance key, compatible provider
family, configuration fingerprint and confirmed scope must match the baseline.
Changing list blocks, `-arg` values in generated text or scan scopes makes missing findings
not verified. Running from another directory changes root names, so findings
show as not verified.

Query fingerprints include the selected workspace. Scope is confirmed from
identity data sources for AWS (account and region), Google including google-beta
(project, region and zone), and AzureRM (subscription and tenant). Each root
that queried these providers runs an additional `terraform plan -refresh=false`
with `-target` for identity data sources, even without `-baseline`. No state
is applied. Cloudflare `-scan` listings confirm scope using their globally
unique parent account or zone ID. Other listings, including Proxmox scans,
plugins and scan blocks without a parent scope, leave missing findings
unverified. A failed identity read also leaves them unverified and adds a note.
Opt-in types `google_project` and `google_folder` also leave missing findings
unverified, since provider identity does not describe what the credentials can see.

User list blocks that reference evaluated values outside `each.key`, `count.index`
or `each.value` over a reference-free `for_each` also leave missing findings unverified.

Older reports without scope leave missing findings unverified once when used
as a baseline. Save a new report to establish confirmed scope. Fingerprints
do not include evaluated variables, locals, data sources or provider bodies;
changes outside the confirmed identity still require care between runs.

Name the environment when one repository checks several (the same roots
with other credentials or workspaces):

```sh
tforphans -environment prod -format json > baseline-prod.json
tforphans -environment prod -baseline baseline-prod.json -strict
```

The name goes into the report (`summary.environment`). When the baseline
has another name, or only one of the two reports has one, nothing is known
or resolved: every finding counts as new and every missing one is not
verified (`environment changed`), with a note saying so. The name is only a
label: tforphans does not check that the credentials really point at that
environment. An automatic `.tforphans.yaml` may set `environment: prod`.

An absent finding may have been deleted, become managed, been ignored or
become uncertain. When the listing that found it ran again unchanged, the
run tells these apart (an object with the same ID from another root,
plugin or changed block may be another one, so those do not count):
the summary line reads, for example, `3 resolved (1 became managed, 1
ignored, 1 no longer found)`, and JSON gives each finding a `transition`
with the state address or ignore rule. A finding that is now uncertain is
not verified (`now uncertain`), even when its listing ran. "Not verified"
otherwise means matching listing evidence is missing,
the listing did not finish, its provider or configuration changed, or the
finding came from a plugin. Plugins are named by position, so a run cannot
show that the same plugin ran again. It is not evidence that the object
disappeared. `-v` prints reasons for the first eight such findings.

Compare saved reports without credentials or Terraform:

```sh
tforphans -format json > current.json
tforphans diff baseline.json current.json
tforphans diff -format json baseline.json current.json
```

`diff` prints added, resolved and not verified findings, changed counters,
coverage status changes and old/new completeness. Lists show eight entries;
`-v` shows all. Exit status is `2` for added findings, `0` otherwise, `1` on
input errors. OLD's and NEW's listing evidence decides which missing findings
are resolved. Older
reports without `summary.listings` leave missing findings not verified.
Both commands read local report paths, reject `-` and require schema version
`1`, an `orphans` array and a single JSON object, at most 1 GiB per file.

The stderr summary also shows each provider's list support and queried type
count, for example `hashicorp/aws: 247 of 1500 resource types can be listed,
240 queried`. JSON separates `summary.states_complete` from
`summary.discovery_complete`, so unread states can be distinguished from
listing gaps. `summary.complete` still requires both.

[Back to the README](../README.md) · [All guides](README.md)
