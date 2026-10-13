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
`-max-state-size 2G`. Zero and negative sizes are invalid. The harmless
`max-state-size` key is also accepted in automatic `.tforphans.yaml` files;
the command line takes precedence.

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

[Back to the README](../README.md) · [All guides](README.md)
