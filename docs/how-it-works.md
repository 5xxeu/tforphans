# How It Works

For every root module directory:

1. `terraform providers schema -json` tells which resource types the
   module's providers can list.
2. A temporary `zz_tforphans_<pid>.tfquery.hcl` with one `list` block
   per type is written next to the configuration, `terraform query -json`
   runs with the module's own provider configuration and credentials, and
   the file is removed.
3. `terraform state pull` gives the module's state, for every workspace
   (selected through `TF_WORKSPACE`, so the module's selected workspace
   stays as it is). Workspaces are taken only from the list lines of
   `terraform workspace list` (`* name`, `  name`), never from other text
   such as backend warnings: pulling a workspace that does not exist makes
   some backends (S3) write an empty state. `-current-workspace` skips the
   list and reads only the selected workspace.

States retain their lineage, serial and source as snapshots. Equivalent
snapshots merge their source lists before analysis. Indexed identities then
find objects owned by several logical states and duplicate addresses within
one state; incomplete identity or lineage makes ownership possible rather
than confirmed. These are warnings, separate from live resource matching;
`-strict-ownership` can fail confirmed ownership and duplicates within a state.

Every listed resource is then compared by
[resource identity](https://developer.hashicorp.com/terraform/plugin/framework/resources/identity)
with all states that were read: the roots' own states plus any `-state`.
A state instance counts only when it belongs to the provider that listed the
object (by provider source; google and google-beta count as one): two
providers can define a type of the same name for different objects.
State written before identities existed (or by providers without them) is
matched by attributes instead; every key of the object's own ID has to
match. Old state often lacks the account or subscription (AWS `account_id`
is no attribute), and so can an identity, so such a match cannot tell an
IAM role `ci` in one account from a role `ci` in another. An empty value in
a listed identity (google lists some objects with `project = ""`) is
unknown too, and such objects from two roots are reported separately, since
they may live in two projects. These matches are reported as uncertain,
not as managed (an error with `-strict`), and a note says how many state
instances have no identity;
`-refresh` reads each root's state through `terraform plan -refresh-only`
instead of `state pull`: the providers return identities for old state,
nothing is written and no lock is taken (the plan file lives in a private
temporary directory and is removed). It costs a full refresh of every root
it reads, `-state-roots` included: every resource is read from the API with
the root's credentials (time and API quota; 1-2 minutes for a root of a few
hundred resources is common) and the root's variables are needed. A line on
stderr says when each root starts and how long it took. When the plan
fails, the stored state is read and a note says so. Identities matter only
for state written before Terraform 1.12; such state is matched by
attributes otherwise, which found the same orphans in the runs so far.
`terraform apply -refresh-only` with a current provider records identities
in state for good (it writes the state, so it needs write access). The boot disk of a managed `google_compute_instance` counts as
managed by it; disks attached with `attached_disk` do not, since they may
be created elsewhere. Whatever matches nothing is reported.

[Back to the README](../README.md) · [All guides](README.md)
