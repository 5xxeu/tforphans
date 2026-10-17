# JSON Output

`-format json` prints one object. `schema_version` (now `1`) goes up when a
field is removed or changes its type or meaning, so a consumer can refuse a
format it does not know; new fields do not change it.

- `orphans`, `uncertain`: objects with `type`, `identity` (the keys state is
  matched on), `display_name`, `scope` (where it lives, `-scan` and some
  list resources), `import_id`, `provider` (local name in the root),
  `provider_source`, `undeclared_provider` (the provider address when
  only child modules declare it, not the root module; empty otherwise),
  `list_address`, `source` (root, `-scan NAME` or
  `pluginN`), `attributes` (only with `-include-attrs`); `uncertain` adds
  `state_address`, the state instance it matched.
- `summary`: counters (`live`, `managed`, `managed_by_identity`,
  `uncertain`, `ignored`, `orphans`, `states`, `queried_types`,
  `api_not_enabled`, `strict_ignored`, `duplicate_ownership`,
  `possible_duplicate_ownership`, `duplicate_in_state`), `complete`,
  `states_complete`, `discovery_complete`, `unread_states`
  (`[{root, error}]`), `failed_sources` (`[{source, error}]`: a root,
  `-scan NAME` or `pluginN` that failed as a whole; the run exits 1 and
  its `listings` are marked incomplete),
  `failed_queries` (`address`, `type`, `scope`,
  `root`, `category`, `message`), `skipped_types` (`type`, `reason`,
  `root`, `excluded`: left out on purpose or without any listing),
  `warnings`, `notes`, `providers_without_list`, `by_type` and `by_scope`
  (counters per type and per scope), `coverage` (`type`, `scope`,
  `status` complete/incomplete/skipped, `reasons`, `live`).
  `listings` is an optional sorted list of actual listing instances, described below.

Empty lists and zero counters marked above as optional may be left out.

## Explanations

With `-explain`, the top-level array `explain` has one entry per live
object in the order they were found: `type`, `identity`, `display_name`,
`scope`, `source`, `status` (`managed`, `uncertain`, `ignored` or
`unmanaged`) and `reason`, a sentence for people whose wording may change.
Managed and uncertain entries add `state_address` and `state_source` (the
root and workspace, or `-state` argument, it was read from); an ignored one
adds `rule`, the file and line or preset of the ignore rule and its text.
Without the flag the array is left out.

## Duplicate state ownership

The optional top-level arrays `duplicate_ownership` and `duplicate_in_state`
contain one entry per object and confidence/reason group (within one state
for `duplicate_in_state`). Each entry has `type`, `provider` (normalized provider
source), `identity` or the legacy `id`, `confidence` (`confirmed` or
`possible`), and `owners`. Confirmed entries omit `reason`; possible ones
use `partial_identity`, `no_identity`, `same_lineage_diverged`, or
`lineage_unknown`.

If any instance marks the legacy ID sensitive, `id` is `"(sensitive)"` and
`id_sensitive` is `true`.

An owner has `address`, `lineage` and `serial` (omitted when unknown), and
`sources`, the sorted list of equivalent state reads. Sources name a root
and workspace as in text output, a redacted `-state` argument, or `-` for
stdin. A serial of zero is known and is included. Several addresses within
one state can appear in a single ownership entry without multiplying the
object count. Legacy ID matches within one state have confidence `possible`
and reason `no_identity`, but still count as `duplicate_in_state`.

The summary adds `duplicate_ownership` (confirmed objects),
`possible_duplicate_ownership`, and `duplicate_in_state` counters, including
zeros. Ownership warnings do not change `summary.complete`, which describes
listing coverage. The schema stays at version `1`.

## Coverage rows and orphan fields

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

## Baselines and diff

`-baseline FILE` reads a previous JSON report from a local path, with the
1 GiB limit and the same `schema_version` as the current report. It requires
one JSON object with an `orphans` array. Each current orphan adds `baseline`:
`"new"` or `"known"`. Without the flag, neither these annotations nor the
top-level `baseline` object is emitted. Summary counters still include all
current findings. The extra object is:

```json
{"new": 3, "known": 40, "resolved": 2, "unverified": 5,
 "resolved_findings": [], "unverified_findings": []}
```

During a run, which sees every live object, a missing finding that the
same listing returned again (same `source` and `list_address`, the same
fingerprint in both reports; never for plugins) says what became of it in
`transition`: `became_managed`
(resolved, with the `state_address` that manages it now), `ignored`
(resolved, with the ignore `rule`) or `uncertain` (unverified, reason
`now uncertain`: state matches it without confirming the account or
cluster). These take precedence over the listing rules below. A finding
resolved by those rules gets `no_longer_found`. `tforphans diff` has only
the two reports and sets no `transition`.

`summary.environment` holds the `-environment` name and is omitted without
it. When the baseline's name differs from the current one (also when only
one of them has a name), the object adds `"environment_changed": true`,
every current orphan is `"new"` and every old finding is unverified with
reason `environment changed`. `diff` then adds
`"environment": {"old": "...", "new": "..."}`, present whenever either
report has a name.

The arrays contain the old findings' `type`, `identity`, `scope`,
`provider_source`, `display_name`, `import_id`, `source` and `list_address`;
`unverified_findings` also has `reason`. Findings match by type, JSON identity,
exact scope and compatible provider family, with an unknown provider matching
any family. An empty identity uses `import_id`. Names and addresses do not
participate. Without both identity and import ID, a current finding is always
new and an old finding stays unverified (`identity and import_id missing`).
A missing finding is resolved only when OLD has listing evidence matching its
`source` and `list_address`, and NEW has the same listing address and source,
a compatible provider family, `complete: true` and the same fingerprint.
Coverage rows and scopes of returned objects do not decide resolution.
Plugin findings whose source matches `plugin` followed by digits always stay
unverified (`plugin results cannot be verified`). Other roots, including
`plugin-infra`, use the normal listing rules.

A missing `list_address` or absent matching OLD listing gives `no listing evidence`,
as does OLD evidence without a fingerprint. No matching NEW listing gives
`not listed this run`. An incompatible provider family gives
`listed by another provider`, then an unfinished or failed listing gives
`listing incomplete`, then a different fingerprint gives
`listing configuration changed`. An empty provider is compatible with any
provider family. Older reports without listing evidence cannot prove resolution.

`tforphans diff -format json OLD.json NEW.json` uses the same rules and emits:

```json
{"added": [], "resolved": [], "unverified": [],
 "counters": {"live": {"old": 1, "new": 2}},
 "coverage_changes": [{"type": "aws_instance", "scope": "account a",
                       "old": "complete", "new": "incomplete"}],
 "complete": {"old": true, "new": false}}
```

`added` contains NEW's orphan objects. `resolved` and `unverified` use the
baseline finding fields above. All seven counters are always included:
`live`, `managed`, `uncertain`, `ignored`, `orphans`, `states`, `queried_types`.
Coverage changes compare status by type and scope; `"absent"` means the row
exists only in the other file. No Terraform process runs.

## Types nothing lists

`summary.unlisted_types`, omitted when empty, lists resource types found in
the states whose provider was queried (its schema read, or results came
from it) but which no listing covered: `type`, `provider` (family) and
`instances`, most instances first. tforphans never checks objects of these
types, so an unmanaged one would not show up. Types left out with `-types`
or `-exclude-types`, and those of providers that create no remote object
(random, null, local, ...), are not listed. A note on stderr names the
first eight; it does not make coverage incomplete.

## Provider coverage and completeness

`summary.providers`, omitted when no provider schema was read, contains
`provider` (the schema's source address), `resource_types`, `list_types` and
`queried_types`. Rows are sorted by provider. Across roots, schema counts use
the maximum and queried types use the distinct union. With `-no-query` this
array is omitted.

`summary.listings`, omitted when no listing ran, contains one record per
listing instance that actually started:

```json
{"source": "infra", "address": "list.google_compute_instance.tfo_google_google_compute_instance[\"project=prod\"]",
 "provider": "hashicorp/google", "type": "google_compute_instance",
 "complete": true, "fingerprint": "<64 hex characters>"}
```

- `source` matches the resource's `source`: its root name or `-scan NAME`.
- `address` matches the resource's `list_address`, including its `for_each`
  instance key. Scans use the data source address and scope instance key,
  such as `data.cloudflare_dns_records.by_zone_id["zone-id"]`.
- `provider` is the normalized compatible family, so `hashicorp/google-beta`
  becomes `hashicorp/google`.
- `type` is the resource type returned by this listing.
- `complete` means this instance finished without a failure counted against
  it. Failures suppressed by `-strict-ignore` still make it false. A failure
  on a whole block counts against its instances.
- `fingerprint` is the hex SHA-256 of canonical JSON containing the block's
  configuration as written, instance key and provider configuration reference
  (local name or alias). User `.tfquery.hcl` blocks use their exact source
  bytes from HCL ranges; generated blocks use their generated block text.
  Scan fingerprints also include the scan name, data source block text and
  scope key, including scan scope restrictions. Any `-arg` value included in
  generated text is therefore covered.

Fingerprints do not include the evaluated values of variables, locals or
data sources, or the provider configuration body. Keep these consistent
between reports. Records are sorted by source, address and type. A root
whose query did not run and a block with no started instances have no records.
`summary.coverage` and provider counts retain their existing meaning.

`summary.states_complete` is true when no state was unread.
`summary.discovery_complete` describes listing coverage, including requested
root queries that could not start. With `-no-query`, an unread state affects
only `states_complete`. Both are always present. `summary.complete` retains its
meaning: both state reading and discovery were complete. Uncertain matches
still fail `-strict` independently.

[Back to the README](../README.md) · [All guides](README.md)
