# Limitations

- Coverage is what providers can list. Check `-v` for skipped types and the
  warnings for failed queries; a type that is not listed cannot be reported.
  A list query that returns as many results as its limit (`-limit` for
  generated blocks, the block's own `limit` for yours, 100 if unset) is
  reported as not fully listed, since there may be more; so is one of your
  blocks whose limit is an expression.
- Some providers list objects they create implicitly. The default presets
  hide those with a reserved mark (see [Ignoring known resources](ignore.md));
  put the rest in the ignore file, or add an opt-in preset.
- An API that is not enabled in a GCP project fails its queries ("API not
  enabled"). Such listings stay in `failed_queries` and are counted in
  `summary.api_not_enabled`, but they do not make coverage incomplete: a
  disabled service shows and accepts no objects, and GCP refuses to disable
  one that has some unless forced. `-strict-api` counts them as gaps.
- The temporary files are written next to the configuration; there is no
  separate work directory, and a provider version cannot be overridden for
  the run (the root's installed providers are used).
- Terragrunt units are read only as states (`-state-roots DIR -terragrunt`);
  their modules are not queried. List their providers' objects from a plain
  root with the same provider configuration, or with `-scan`.
- Generated configuration can need manual fixes when the API does not return
  everything the resource requires. For example, Cloudflare does not return
  a Worker's code, so `cloudflare_workers_script` needs `content_file`, and
  `cloudflare_account_member` needs `roles` or `policies`.

- Ownership detection compares only managed instances in states that were
  successfully read, not plugin or `-scan` results. It identifies ownership
  recorded in state, without proving that the object still exists remotely.
  Types must match exactly and providers must match by the same provider
  families used for live matching (including google/google-beta and
  hashicorp/github/integrations/github). Unknown providers cannot distinguish
  provider families.
- Equivalent snapshots merge only when lineage, serial and normalized managed
  content match; snapshots without a lineage (`-refresh`) merge only when the
  same source was read again, since copied roots with separate states can
  hold identical content. Formatting, unrelated attributes
  and metadata do not affect equivalence. Changed addresses do affect it.
  Copies with the same lineage but diverged serial or content are possible
  duplicates (`same_lineage_diverged`), not confirmed independent ownership.
- Full identities must have equal key sets and equal non-empty JSON values.
  Missing keys, empty strings, nulls and empty lists or maps make a match
  possible (`partial_identity`); conflicting known values do not match.
  A partial match requires at least one equal known identity value and exactly
  one compatible grouped object for the less complete identity; ambiguous
  identities and identities with no known values do not match. Equal legacy IDs
  are only possible ownership (`no_identity`) because scope may be absent.
  Unknown lineage takes precedence (`lineage_unknown`); `-refresh` uses show
  JSON, which has no lineage or serial, so refreshed ownership is possible.
- Resources from hashicorp/null, random, time, local, tls, archive, external,
  http and cloudinit, and built-in `terraform_data`, are excluded from
  ownership checks: equal IDs need not represent a shared remote object.

[Back to the README](../README.md) · [All guides](README.md)
