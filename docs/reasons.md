# Reason codes

Machine consumers should use `reason_code`, not the human wording of `reason`.
Codes below are the stable enum for audit reports (JSON `schema_version: 1`).
New codes may be added; consumers should preserve unknown codes and fall back to
`reason` rather than treating them as success. Codes describe evidence, not
instructions to perform writes automatically.

`action` is optional and is one of `refresh_state`, `raise_limit`, `enable_api`,
or `grant_permission`. It identifies a remedy only when the cause is specific.
It is a hint for a person, not authorization to change infrastructure.
`missing_keys` is optional and contains sorted, unique key names, never values.
The field is omitted when those names are unknown.

`explain`, `summary.skipped_types`, and baseline `unverified_findings` carry
these fields beside `reason`. Coverage can have several causes:
`summary.coverage[].reason_details` contains `{reason, reason_code, action?,
missing_keys?}`, sorted by reason, then code and action. Distinct codes or
actions with the same wording are retained as separate entries; repeated
causes merge their key names. Complete coverage rows omit both arrays. `diff` findings use the same fields.

<a id="scope_unverified"></a>

## scope_unverified

The account or cluster cannot be confirmed. Check that the states and discovery refer to the same environment. Trust flags are appropriate only after checking that relationship.

<a id="no_identity"></a>

## no_identity

Identity or import ID is unavailable. Matching or verifying the object is not possible with this evidence.

<a id="state_identity_missing"></a>

## state_identity_missing

State lacks identity keys needed to confirm the match. `action: refresh_state`: use `-refresh`, or record identities with `terraform apply -refresh-only` and a provider that supports them.

<a id="discovery_identity_missing"></a>

## discovery_identity_missing

Discovery returned empty identity keys. Check the listing and provider configuration; refreshing state alone cannot supply these values.

<a id="api_disabled"></a>

## api_disabled

The API is disabled. `action: enable_api`: enable it and repeat the audit. A disabled API can still retain resources.

<a id="permission_denied"></a>

## permission_denied

The listing was denied. `action: grant_permission`: grant the required read permissions and repeat the audit.

<a id="limit_reached"></a>

## limit_reached

Results may be truncated. `action: raise_limit`: raise the relevant list limit or adapter `max_items`, and repeat the audit.

<a id="environment_changed"></a>

## environment_changed

The baseline and current report have different environment labels. Choose reports for the same environment.

<a id="missing_argument"></a>

## missing_argument

The listing needs an argument or configuration block. `missing_keys`, when known, names the missing inputs. Supply supported query arguments with `-arg`; adapter limitations may require another listing method.

<a id="invalid_argument"></a>

## invalid_argument

A supplied argument is unsupported for this listing. Check `-arg` and the provider configuration.

<a id="opt_in_required"></a>

## opt_in_required

This type requires its exact name in `-types` before it is listed.

<a id="provider_ambiguous"></a>

## provider_ambiguous

The provider has several local names and its configuration cannot be selected reliably. Give it one local name.

<a id="aliases_disabled"></a>

## aliases_disabled

`-no-aliases` prevented querying this provider configuration.

<a id="equivalent_listing"></a>

## equivalent_listing

An equivalent type or provider configuration already lists these objects.

<a id="adapter_excluded"></a>

## adapter_excluded

The adapter deliberately excludes this type. The accompanying reason explains its mapping or limitation.

<a id="listing_unsupported"></a>

## listing_unsupported

No supported list resource or plural data source exists for this type.

<a id="result_schema_unsupported"></a>

## result_schema_unsupported

The provider schema lacks a result list or field required by the adapter.

<a id="scope_empty"></a>

## scope_empty

No parent scope was returned in which to read this type. Check visibility and credentials.

<a id="scope_excluded"></a>

## scope_excluded

The selected `-scan-scope` values leave this scope out.

<a id="adapter_unsupported"></a>

## adapter_unsupported

The adapter recognized a provider limitation and skipped the listing.

<a id="not_found"></a>

## not_found

The API returned not found. This does not prove the API is disabled or that the resource is absent.

<a id="provider_crashed"></a>

## provider_crashed

The provider crashed while listing. Inspect the failure message and provider version.

<a id="api_retired"></a>

## api_retired

The listing uses an API that has been retired.

<a id="provider_error"></a>

## provider_error

The listing failed because of a recognized provider error.

<a id="request_rejected"></a>

## request_rejected

The API rejected the request. Check the reported error and product availability.
A 403 whose body says the account lacks the product, entitlement or quota
(Cloudflare: Advanced Certificate Manager, SSL for SaaS, Magic Transit,
Workers for Platforms, DEX) is classified here, not as `permission_denied`:
a broader token would not help. A 403 without such a body stays
`permission_denied`.

<a id="listing_failed"></a>

## listing_failed

The listing failed without a more specific classified cause.

<a id="listing_skipped"></a>

## listing_skipped

The listing was skipped without more specific metadata. This is the fallback for callers supplying skips without codes.

<a id="state_match"></a>

## state_match

The object has a confirmed state match.

<a id="ignore_rule"></a>

## ignore_rule

An ignore rule hides this finding. `rule` identifies the rule.

<a id="no_state_instance"></a>

## no_state_instance

No state instance of this resource type was read.

<a id="identity_not_matched"></a>

## identity_not_matched

None of the state instances of this type shares the identity.

<a id="identity_conflict"></a>

## identity_conflict

Nearby state instances were rejected because their keys or scope conflict.

<a id="plugin_unverifiable"></a>

## plugin_unverifiable

A missing baseline plugin result cannot be verified by rerunning the plugin.

<a id="listing_evidence_missing"></a>

## listing_evidence_missing

The baseline has no listing address or fingerprint proving where this object was found.

<a id="listing_not_run"></a>

## listing_not_run

The original listing did not run this time.

<a id="provider_changed"></a>

## provider_changed

The listing now uses a different provider family.

<a id="listing_incomplete"></a>

## listing_incomplete

The original listing did not finish completely this time.

<a id="configuration_changed"></a>

## configuration_changed

The listing configuration or execution context changed since the baseline.

<a id="scope_changed"></a>

## scope_changed

The listing account, project, region or other scope changed since the baseline.
