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

# vault stderr goes to a file of its own: mixed into stdout, a warning would
# make the JSON unreadable.
err=$(mktemp) || exit 1
trap 'rm -f "$err"' EXIT
trap 'exit 1' HUP INT TERM

# resource TYPE ID: one resource line.
resource() {
	jq -nc --arg t "$1" --arg id "$2" '{type: $t, identity: {id: $id}, display_name: $id, import_id: $id, unverified: true}'
}

fail() {
	jq -nc --arg e "$1" '{error: $e}'
}

# emit WHAT JSON FILTER [JQ ARGS]: the lines FILTER makes of JSON, or, if
# jq fails, an error line and nothing else (sh has no pipefail, so its
# status is checked here).
emit() {
	what=$1 in=$2
	shift 2
	if lines=$(printf '%s' "$in" | jq -c "$@" 2>"$err"); then
		[ -z "$lines" ] || printf '%s\n' "$lines"
	else
		fail "$what: unexpected output: $(cat "$err")"
	fi
}

# roles PATH TYPE: the roles of an auth mount, or an error line.
roles() {
	if out=$(vault list -format=json "auth/$1/role" 2>"$err"); then
		emit "list auth/$1/role" "$out" --arg p "$1" --arg t "$2" \
			'.[] | ("auth/" + $p + "/role/" + .) as $id | {type: $t, identity: {id: $id}, display_name: $id, import_id: $id, unverified: true}'
		return
	fi
	case $(cat "$err") in
	*"No value found"*) ;; # no roles
	*) fail "list auth/$1/role: $(cat "$err")" ;;
	esac
}

if auth=$(vault read -format=json sys/auth 2>"$err"); then
	# One compact JSON object per line; jq escapes anything inside names.
	if mounts=$(printf '%s' "$auth" | jq -c '.data | to_entries[] | select(.value.type != "token")
		| {path: (.key | rtrimstr("/")), type: .value.type}' 2>"$err"); then
		printf '%s\n' "$mounts" |
			while IFS= read -r mount; do
				[ -n "$mount" ] || continue
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
		fail "read sys/auth: unexpected output: $(cat "$err")"
	fi
else
	fail "read sys/auth: $(cat "$err")"
fi

if out=$(vault read -format=json sys/mounts 2>"$err"); then
	emit "read sys/mounts" "$out" '.data | to_entries[]
		| select(.value.type | IN("system", "cubbyhole", "identity") | not)
		| (.key | rtrimstr("/")) as $id
		| {type: "vault_mount", identity: {id: $id}, display_name: $id, import_id: $id, unverified: true}'
else
	fail "read sys/mounts: $(cat "$err")"
fi

if out=$(vault policy list -format=json 2>"$err"); then
	emit "list policies" "$out" '.[] | select(. != "default" and . != "root")
		| {type: "vault_policy", identity: {id: .}, display_name: ., import_id: ., unverified: true}'
else
	fail "list policies: $(cat "$err")"
fi
