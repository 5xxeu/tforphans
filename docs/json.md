# JSON Output

`-format json` prints one object. `schema_version` (now `1`) goes up when a
field is removed or changes its type or meaning, so a consumer can refuse a
format it does not know; new fields do not change it.

- `orphans`, `uncertain`: objects with `type`, `identity` (the keys state is
  matched on), `display_name`, `scope` (where it lives, `-scan` and some
  list resources), `import_id`, `provider` (local name in the root),
  `provider_source`, `list_address`, `source` (root, `-scan NAME` or
  `pluginN`), `attributes` (only with `-include-attrs`); `uncertain` adds
  `state_address`, the state instance it matched.
- `summary`: counters (`live`, `managed`, `managed_by_identity`,
  `uncertain`, `ignored`, `orphans`, `states`, `queried_types`,
  `api_not_enabled`, `strict_ignored`, `duplicate_ownership`,
  `possible_duplicate_ownership`, `duplicate_in_state`), `complete`, `unread_states`
  (`[{root, error}]`), `failed_queries` (`address`, `type`, `scope`,
  `root`, `category`, `message`), `skipped_types` (`type`, `reason`,
  `root`, `excluded`: left out on purpose or without any listing),
  `warnings`, `notes`, `providers_without_list`, `by_type` and `by_scope`
  (counters per type and per scope), `coverage` (`type`, `scope`,
  `status` complete/incomplete/skipped, `reasons`, `live`).

Empty lists and zero counters marked above as optional may be left out.

## Duplicate state ownership

The optional top-level arrays `duplicate_ownership` and `duplicate_in_state`
contain one entry per object and confidence/reason group (within one state
for `duplicate_in_state`). Each entry has `type`, `provider` (normalized provider
source), `identity` or the legacy `id`, `confidence` (`confirmed` or
`possible`), and `owners`. Confirmed entries omit `reason`; possible ones
use `partial_identity`, `no_identity`, `same_lineage_diverged`, or
`lineage_unknown`.

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

[Back to the README](../README.md) · [All guides](README.md)
