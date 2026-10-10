#!/bin/sh
# tforphans plugin for HashiCorp Vault, whose provider has no list
# resources. Prints auth methods, auth roles, secret engines and ACL policies
# as JSON lines.
#
# Needs the vault CLI (VAULT_ADDR, VAULT_TOKEN, ...) and jq. Usage:
#   tforphans -no-query -state <vault state> -plugin examples/plugins/vault.sh
#
# Vault state does not record the Vault address, so names are marked
# "unverified": they match state only as unverified unless -plugin-cluster-states
# says that the states given belong to this Vault.
#
# Names come from Vault and are treated as data: every JSON line is built by
# jq from --arg values, never by the shell or sed.
set -u

# resource TYPE ID: one resource line.
resource() {
	jq -nc --arg t "$1" --arg id "$2" '{type: $t, identity: {id: $id}, display_name: $id, import_id: $id, unverified: true}'
}

fail() {
	jq -nc --arg e "$1" '{error: $e}'
}

# roles PATH TYPE: the roles of an auth mount, or an error line.
roles() {
	if out=$(vault list -format=json "auth/$1/role" 2>&1); then
		printf '%s' "$out" | jq -c --arg p "$1" --arg t "$2" \
			'.[] | ("auth/" + $p + "/role/" + .) as $id | {type: $t, identity: {id: $id}, display_name: $id, import_id: $id, unverified: true}'
		return
	fi
	case $out in
	*"No value found"*) ;; # no roles
	*) fail "list auth/$1/role: $out" ;;
	esac
}

if auth=$(vault read -format=json sys/auth 2>&1); then
	# One compact JSON object per line; jq escapes anything inside names.
	printf '%s' "$auth" | jq -c '.data | to_entries[] | select(.value.type != "token")
		| {path: (.key | rtrimstr("/")), type: .value.type}' |
		while IFS= read -r mount; do
			path=$(printf '%s' "$mount" | jq -r .path)
			type=$(printf '%s' "$mount" | jq -r .type)
			case $type in
			jwt | oidc)
				resource vault_jwt_auth_backend "$path"
				roles "$path" vault_jwt_auth_backend_role
				;;
			kubernetes)
				resource vault_auth_backend "$path"
				roles "$path" vault_kubernetes_auth_backend_role
				;;
			approle)
				resource vault_auth_backend "$path"
				roles "$path" vault_approle_auth_backend_role
				;;
			*) resource vault_auth_backend "$path" ;;
			esac
		done
else
	fail "read sys/auth: $auth"
fi

if mounts=$(vault read -format=json sys/mounts 2>&1); then
	printf '%s' "$mounts" | jq -c '.data | to_entries[]
		| select(.value.type | IN("system", "cubbyhole", "identity") | not)
		| (.key | rtrimstr("/")) as $id
		| {type: "vault_mount", identity: {id: $id}, display_name: $id, import_id: $id, unverified: true}'
else
	fail "read sys/mounts: $mounts"
fi

if policies=$(vault policy list -format=json 2>&1); then
	printf '%s' "$policies" | jq -c '.[] | select(. != "default" and . != "root")
		| {type: "vault_policy", identity: {id: .}, display_name: ., import_id: ., unverified: true}'
else
	fail "list policies: $policies"
fi
