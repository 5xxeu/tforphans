# Providers without List Resources: `-scan`

Many providers that have no list resources yet have plural data sources
(`cloudflare_dns_records` next to `cloudflare_dns_record`), which list
objects with the provider's own API code and authentication. `-scan NAME`
uses them:

```sh
export CLOUDFLARE_API_TOKEN=...   # read-only, "Read all resources" template
tforphans -no-query -scan cloudflare -state-roots ~/work/cloudflare
```

For each resource type with a plural data source, a temporary root module
gets a data block (iterating over the parent zones or accounts when the
data source takes `zone_id` or `account_id`), and `terraform apply
-refresh-only` reads them into a local state in a temporary directory. Data
sources only read, nothing is changed. Credentials come from the provider's
environment variables, since the temporary root has no provider block. A
data source that returns as many objects as its `max_items` (1000000) may
have stopped there: its type counts as not fully listed, and so does every
type listed per zone or account of a parent list that hit the cap.

The provider version is the user's: the lock file and the installed
provider of the first root (given or found under `-state-roots`) that pins
the provider are reused (the data directory is `TF_DATA_DIR` when set); if
that root has the lock file but no installed provider, exactly the locked
version is downloaded and checked against the lock file's checksums (only
the provider's own lock entry is used, with `-lockfile=readonly`). Import
blocks use the provider's local name in that root (`provider = cf` for
`cf = { source = "cloudflare/cloudflare" }`), but the objects were found
with the provider configured from the environment, and the root's own
provider block may point to another account or cluster. Each such import
block says so in a comment: check it before `terraform plan`. Without such a root the scan stops: a provider
release nobody chose would run with your credentials. `-allow-unpinned`
uses the newest release instead, cached under the user cache directory.

A data source that fails (missing token permission, a product the account
does not have, a provider bug) is reported as "not fully listed" and the
others are read again, so one failure does not stop the scan. A failure in
one zone or account only takes that zone or account out; the others are
still listed. When the provider or terraform crashes (a panic names no
data source), the data sources are read again in halves until the crash
is pinned to a data source and, for those read per zone or account, to the
zones or accounts that crash; those are reported as "crashed" and the rest
is listed as usual. Where the adapter knows a filter that splits a listing
(Cloudflare DNS records by `type`), a zone that crashes is read again once
per value, and only the values that still crash are reported, for example
`data.cloudflare_dns_records.by_zone_id_by_type["<zone>/NAPTR"]` with
`type NAPTR: crashed: ...`. This takes extra runs (at most 80; what is still
suspect after them is reported as crashed), and a note says so while it
happens. Before that, three unrelated data sources (under different parents)
are read one at a time; when each of them crashes too, the cause is most
likely the environment (network, the provider plugin, terraform) rather than
one type, so nothing is split: every data source is reported as crashed with
that reason, and the scan is worth running again. With
`-strict` such gaps are errors; leave out products you do not use with
`-exclude-types`.

An adapter per provider holds what the schema cannot say: which objects the
provider creates on its own, which types are the same object, how import IDs
look. Adapters exist for Cloudflare and Proxmox VE.

## Cloudflare

For Cloudflare (5.x) the adapter knows that:

- objects Cloudflare creates and owns are not reported, judged only by
  fields users cannot set (a name or a flag a user can set could hide a
  hand-made object): records of Workers custom domains and Email Routing,
  Cloudflare's managed rulesets (by their global IDs), Universal SSL
  certificates, managed Gateway certificates, built-in DLP entries, the
  default device profile, the catch-all rule. The default virtual network
  and the Gateway CA mTLS certificate are reported; ignore them if wanted;
- objects listed in several scopes or under several types are reported once;
- 4.x type names (`cloudflare_record`, `cloudflare_tunnel`, ...) and
  `cloudflare_worker` next to `cloudflare_workers_script` count as managing
  the same object.

State is matched by `id` plus `zone_id` or `account_id`. Import IDs follow
the provider's convention (`<zone_id>/<id>`, or `accounts/<account_id>/<id>`
for resources that can live in an account or a zone).

**Coverage.** A type is listed when the provider has a plural data source
for it that needs no argument beyond `zone_id` or `account_id`. On a small
account that is about 95 of the 271 resource types (cloudflare 5.x), read
in under a minute; most of the rest are settings and singletons (zone
settings, `*_settings`, one-per-zone objects), which cannot be orphaned, or
child objects whose data source needs a parent ID (list items, access
policies of one application). `-v` lists every skipped type with the
reason, and `summary.skipped_types` in JSON.

**Token.** The scan reads `CLOUDFLARE_API_TOKEN` (or the provider's other
environment variables), never the root's provider block: that block can
use files, variables and functions that only a plan of your configuration
evaluates. `-scan` does not plan the root's configuration and takes its token
from the environment. Targeted identity plans run only for AWS, Google
(including google-beta) and AzureRM query listings; they do not involve
Cloudflare. If your configuration reads the token from a file, pass it the
same way:

```sh
CLOUDFLARE_API_TOKEN="$(jq -r .api_token ~/.cloudflare.json)" \
  tforphans -no-query -scan cloudflare -state-roots ~/work/cloudflare
```

Create the token from the "Read all resources" template (Account and Zone
read permissions on all accounts and zones). Types the token cannot read
are reported as "permission denied" with their names; Cloudflare's 403
does not say which permission was missing, and the permission names do not
map one to one onto resource types, so add the read permission of the
product in question (for example "Logs Read" for `cloudflare_logpush_job`,
"API Tokens Read" for `cloudflare_api_token`). Products the account's plan
does not include usually answer "400 Bad Request" ("request rejected"),
and retired APIs (the old firewall rules and filters) "410 Gone"
("API retired"); exclude such types with `-exclude-types`.

**Several accounts.** One token, one account set: run tforphans once per
account (staging, prod), each with its own token and the roots of that
account under `-state-roots`. Objects of an account whose state was not
read show up as unmanaged.

**Scope.** The scan lists whole accounts. Compared with the state of one
stack, everything that other stacks manage is reported too, so give all
states with `-state-roots`, or limit the scan:

```sh
tforphans -no-query -scan cloudflare -scan-scope zone_id=example.com,example.org -scan-scope account_id=
tforphans -no-query -scan cloudflare -scan-scope state -state-roots stacks/web
```

The zone and account limits are independent: `zone_id=...` alone still
lists the account-level objects of every account, and `account_id=A` does
not limit the zones to those of account A. The first line above lists only
the two zones (`account_id=` with no value leaves out account-level
objects).

`-scan-scope zone_id=...` (or `account_id=...`) takes IDs or names and lists
only those zones (or accounts): their objects, and the zones themselves.
`-scan-scope state` takes the zone and account IDs found in the states
read; a scope with no ID in the states is not read at all (its types are
reported as skipped). A zone stack whose state names its account (an
account-level object, or any `account_id`) gets the account's objects
listed as well; `-scan-scope account_id=` (no value) leaves out
account-level objects, and works with `state` for the zones:
`-scan-scope state -scan-scope account_id=`. Some zone listings return
account objects too (Access applications); the account listing of such a
data source is still read, only to drop them, and if it fails the type
counts as not fully listed.
Without `-scan-scope` and `-state-roots` a note points this out. Every
object carries its zone or account (`scope` in JSON, a column in the
table), and `-format summary` counts them per zone.

**Known noise.** Objects Cloudflare creates are left out by fields users
cannot set (see above), and also the resource library catalog
(`cloudflare_zero_trust_resource_library_application` with
`application_source = "cloudflare"`). The default preset `cloudflare-system`
also hides the URL normalization entrypoint ruleset (created by
`cloudflare_url_normalization_settings`). Reported on purpose, ignore them
yourself if you accept them: the default virtual network and the Gateway CA
mTLS certificate (their marks are settable by users), and "default"
entrypoint rulesets and notification policies, which the dashboard creates
when someone configures rules or alerts there (configuration outside
Terraform), for example `cloudflare_notification_policy:Default*`.

**Crashes.** Some provider versions return values that crash terraform for
some objects. DNS records whose data differ in shape (CAA and NAPTR in one
zone, cloudflare/terraform-provider-cloudflare#5581) are then read one
record type at a time, the values taken from the provider schema, so the
zone is still listed in full. Other crashes cost only the zones they happen
in (see above); report them to the provider with the message from `-v`.

## Proxmox VE

`-scan proxmox` covers `bpg/proxmox`: VMs, containers, pools, users, groups,
roles, HA groups and resources, replication jobs, SDN vnets, backup jobs and
ACME accounts. Guests are matched by VM ID, which is unique in a cluster
and survives migration; `telmate/proxmox` resources
(`proxmox_vm_qemu`, `proxmox_lxc`) and the deprecated
`proxmox_virtual_environment_*` names of renamed types count as managing the
same objects. Built-in roles and `root@pam` are not reported. Import IDs
are `<node>/<vm_id>` for guests and the object ID otherwise. Guests are
shown as `name (node)`, since template names repeat per node; ignore rules
by name still match the bare name.

Credentials come from the environment only: `PROXMOX_VE_ENDPOINT` and
either `PROXMOX_VE_API_TOKEN` (`user@realm!token=secret`, a token with the
PVEAuditor role is enough) or `PROXMOX_VE_USERNAME` and
`PROXMOX_VE_PASSWORD`, plus `PROXMOX_VE_INSECURE=true` for a self-signed
certificate. The provider block of your root is not used, as with every
`-scan`: the data sources are read in a temporary root of their own, so
that nothing in your configuration (or its state lock) is touched. No
`ROOT_DIR` is needed: in a directory without Terraform files `-scan` runs
alone, as with `-no-query`. Before the listings, one cheap request (the
version) checks the credentials: if they are rejected (401), the run stops
with `authentication failed` and exit 1, so a wrong password costs one
failed login, not one per data source (which could trip fail2ban or lock
the account). On Proxmox VE 9, which replaced HA groups with HA rules, HA
groups are skipped (left out, not a gap); HA rules have no plural data
source to list them.

Proxmox state does not record which cluster an object belongs to, and VM
IDs repeat across clusters. A guest never matches state whose node
(`node_name`, telmate's `target_node`) the scanned cluster does not have.
A node it does have proves nothing, though: node names such as `pve1`
repeat across clusters too. So all Proxmox matches are uncertain (an error
with `-strict`) unless you state that every state given belongs to the
scanned cluster:

```sh
tforphans -no-query -scan proxmox -state-roots ~/work/proxmox/cluster-a \
  -proxmox-cluster-states -strict
```

Scan one cluster per run, with that cluster's states only.

[Back to the README](../README.md) · [All guides](README.md)
