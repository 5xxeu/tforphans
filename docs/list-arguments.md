# Regions, Zones and Other List Arguments

List queries use the provider configuration of the root module, so by
default AWS is listed in the provider's region only. `-arg` fans every list
query whose config has that string argument out over several values:

```sh
tforphans -arg region=eu-north-1,eu-central-1,us-east-1
tforphans -arg location=global,europe-north1   # google
```

An argument can be limited to some types with `TYPE_GLOB:`, and then wins
over the same argument without one for those types; the values of several
such arguments that match a type are combined. Google Compute
instances, disks and snapshots need a zone, which the list config does not
require (the query fails with "Cannot determine zone" unless the provider
block sets one), so the first scan of a GCP project usually needs:

```sh
tforphans -arg location=global,europe-west1 -arg 'google_compute_*:zone=auto'
```

`zone=auto` and `region=auto` (google and google-beta) stand for every zone
or region: a temporary `zz_tforphans_<pid>_auto.tf` in the root reads them
with the data sources `google_compute_regions` and `google_compute_zones`,
through the root's own provider configuration (its project), and the
queries fan out over what they return. Nothing is missed for a zone that
nobody thought of, at the price of one query per zone and type (GCP has
about 130 zones). When the zones cannot be read,
the queries that need them fail with that error, the others still run; a
disabled Compute API counts as one (see `-strict-api`). A shorter list
is still possible:

```sh
tforphans -arg "google_compute_*:zone=$(gcloud compute zones list --filter='region:europe-west1' --format='value(name)' | paste -sd, -)"
```

Types whose list config has a required argument that no `-arg` supplies (for
example `rest_api_id` for API Gateway children) are skipped, because one
invalid list block makes Terraform reject the whole query. `-v` lists them
with the reason.

When a root uses both `google` and `google-beta`, a type both can list is
queried once, through the provider the root declares, preferring `google`.
Types whose lists reach beyond what a root manages (`google_project` lists
every project the credentials can see) are skipped unless `-types` names
them exactly. `-v` shows, per root, which projects and accounts the results
came from.

A root pinned to an older Terraform (`required_version = "= 1.10.5"`)
refuses `terraform query`; `-ignore-required-version` lifts the root
module's constraint for each command with a temporary
`zz_tforphans_<pid>_version_override.tf` (child modules' constraints still
apply, and an override file of yours that sorts after it is an error, since
it could set the constraint again).

For anything more specific (filters, accounts no provider block of the root
reaches), put your own `list` blocks in a `*.tfquery.hcl` file in the root
module. They run together with the generated ones and their results are
checked the same way.

Generated blocks list through the provider's default configuration and
through each aliased one (`provider "aws" { alias = "prod" }` gets blocks
with `provider = aws.prod`), since each may look at another account. Their
results carry that configuration: import blocks from `adopt` name it, and
coverage counts each configuration apart, so a type that one alias could
not list (a missing argument) is a gap even when the default listed it.
A root that configures a provider only with aliases is listed through the
aliases alone: its default would be an empty configuration that could
reach another account through the environment (a note says so).
`-no-aliases` lists through default configurations only; every type of an
alias then counts as not checked (coverage incomplete, an error with
`-strict`). A provider with no provider block at all is queried
(credentials from the environment, as for any run). Each configuration adds
a full set of queries, so a root with three aliases of a provider takes
about four times as long for it.

## Narrowing the scan

```sh
tforphans -types 'aws_s3_*,aws_iam_*'
tforphans -exclude-types 'aws_cloudwatch_log_*'
tforphans -print-query      # show the temporary files (list blocks and helpers) and exit
```

[Back to the README](../README.md) · [All guides](README.md)
