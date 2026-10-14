# Output, Exit Status and Coverage

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
A summary line counts new, known, resolved and not verified findings.
`-strict` still exits `1` for incomplete coverage or uncertain matches,
even when every orphan is known. `adopt` does not accept `-baseline`.
An automatic `.tforphans.yaml` may set `baseline: baseline.json`, with the
same path restrictions as `ignore`.

A previous finding that is absent now is resolved only when the same root
or scan, with a compatible provider, fully listed its type again this run.
A finding's scope must appear in the listing's generated or scan scopes, or
in objects returned by that same source, provider family and type this run.
Without an explicit scope, when the last object of a scope is deleted, its finding shows as not verified rather than resolved, because nothing proves that scope was listed.
Running from another directory changes root names, so findings show as not
verified. It may have been deleted, become managed, been ignored or become
uncertain. "Not verified" means the current run lacks matching listing
evidence, did not fully list that type or scope, another provider with the
same type names listed it in the same source, or it came from a plugin.
Plugins are named by position, so a run cannot show that the same plugin ran
again. It is not evidence that the object disappeared. `-v` prints reasons
for the first eight such findings. Keep discovery options consistent when
updating the baseline.

Compare saved reports without credentials or Terraform:

```sh
tforphans -format json > current.json
tforphans diff baseline.json current.json
tforphans diff -format json baseline.json current.json
```

`diff` prints added, resolved and not verified findings, changed counters,
coverage status changes and old/new completeness. Lists show eight entries;
`-v` shows all. Exit status is `2` for added findings, `0` otherwise, `1` on
input errors. NEW's listing evidence decides which missing findings are resolved. Older
reports without `summary.listings` leave missing findings not verified.
Both commands read local report paths, reject `-` and require schema version
`1`, an `orphans` array and a single JSON object, at most 1 GiB per file.

The stderr summary also shows each provider's list support and queried type
count, for example `hashicorp/aws: 247 of 1500 resource types can be listed,
240 queried`. JSON separates `summary.states_complete` from
`summary.discovery_complete`, so unread states can be distinguished from
listing gaps. `summary.complete` still requires both.

[Back to the README](../README.md) · [All guides](README.md)
