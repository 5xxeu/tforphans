# Ignoring Known Resources

`.tforphans-ignore` in the working directory (or `-ignore FILE`)
holds `TYPE_GLOB[:NAME_GLOB] [KEY=GLOB...]` lines. `*` matches any
characters. The name glob is tried against the display name (and the bare
name where it adds to it, like the node of a Proxmox guest), the import ID
and every identity value. Each `KEY=GLOB` must match as well: an identity
key (`project=prod-*`), or `attr.` and a path into the resource's
attributes (`attr.labels.goog-gke-node=*`), which tforphans then fetches for
that rule's types (`include_resource`, slower) but does not print. Your
own list blocks need `include_resource = true` for that (a note says so).
Results of `-scan` carry the fields their data source returns; plugin
results have no attributes:

```
# AWS-managed service roles
aws_iam_role:AWSServiceRole*
# created by the platform team's own tooling
aws_s3_bucket:platform-*
# everything of a type
aws_default_vpc
# one project only
google_compute_disk name=pvc-* project=data-prod
```

Built-in presets for objects the cloud creates on its own, known by marks a
user cannot give their own objects, are applied by default:
`aws-system`, `cloudflare-system` and `gcp-system`. What they hide counts as
ignored, the `plan:` line says they are on, and `-no-default-presets` shows
those objects again. Presets that go by names or flags anyone can set
(`gcp-gke-names`, `proxmox-templates`) are applied only with
`-ignore-preset NAME`; when one of them would hide 5 or more unmanaged
objects, a note names it (counting only by the fields at hand: rules on
attributes that were not fetched, such as firewall descriptions, are not
tried for the note).

`aws-system` hides the default security group of every VPC (AWS creates it,
and `default` is a reserved group name; it is listed both as
`aws_default_security_group` and as `aws_security_group`) and the aliases
of AWS managed keys (`alias/aws/*`). The AWS provider's list resources
already leave out default VPCs, subnets and network ACLs and service-linked
roles. AWS managed KMS keys, main route tables and IAM Identity Center roles
have no such mark and stay; see
[the preset](../internal/report/presets/aws-system.ignore).

`gcp-system` hides objects that Google
Cloud and GKE create on their own, by marks a user cannot give their own
objects: GKE nodes and disks by their reserved `goog-` labels, default
service accounts and App Engine buckets in Google's own domains. See
[the preset](../internal/report/presets/gcp-system.ignore). Other system
objects are known only by name (`gke-*` and `k8s-*` firewall rules and load
balancer parts, `gcf-sources-*` and `*_cloudbuild` buckets), and anyone who
can create such an object can choose that name, so the preset leaves them
out. Add them to your own ignore file if that risk is acceptable, ideally
scoped to a project:

```
google_compute_firewall name=gke-* project=my-gke-project
google_compute_firewall name=k8s-* project=my-gke-project
google_storage_bucket name=gcf-sources-*
```

`-ignore-preset gcp-gke-names` hides the firewall rules and health checks
that GKE and its load balancer controllers create (`gke-*`, `k8s-fw-*`,
`k8s-*-node-hc`, `k8s2-*`, and rules whose description names a Kubernetes
service), by exactly those names and descriptions: anyone who can create
a firewall rule can hide it this way, so it is a separate, explicit choice.
See [the preset](../internal/report/presets/gcp-gke-names.ignore).

`cloudflare-system` is the same for `-scan cloudflare`; see
[Cloudflare](scan.md#cloudflare) for what it covers. `-ignore-preset
proxmox-templates` hides Proxmox VM and container templates (by their
`template` flag); use it where templates are built outside Terraform.

When 20 or more unmanaged objects share a type and a scope (certificate
packs ordered outside Terraform), a note prints the ignore line that would
hide them. Objects with a `type` attribute of their own (advanced or
uploaded certificate packs, DNS record types) are counted per kind, and the
line names it, such as `cloudflare_certificate_pack zone_id=<id>
attr.type=advanced`, so that it does not hide a later object of another
kind there. It does hide later objects of that kind, so add it only when
they are expected. Only `universal` certificate packs are dropped on their
own: for advanced packs, neither the type nor the certificate authority nor
the validation method tells one a tool ordered from one a Terraform root
should manage.

[Back to the README](../README.md) · [All guides](README.md)
