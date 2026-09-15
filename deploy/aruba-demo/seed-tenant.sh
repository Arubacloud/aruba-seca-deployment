#!/usr/bin/env bash
# Add ONE tenant to an ALREADY-DEPLOYED aruba demo stack.
#
# This is the onboarding half of install.sh, and nothing else. It touches:
#
#   * the tenant's Vault credential  kv/<tenant>          (aruba-system, dev-mode Vault)
#   * the tenant's namespace, SKU catalogs, Role and RoleAssignment  (tenant.yaml)
#
# It does NOT run helm, does not touch the gateways, the operator, the delegator or
# the cluster-scoped Region CR. Every object it writes is scoped to one tenant, so
# running it cannot disturb a tenant that is already working.
#
# Use it when a second Aruba account wants to drive the same cluster. Use install.sh
# instead to stand the stack up in the first place.
#
#   KUBE_CONTEXT=… ARUBA_TENANT=ARU-123456 \
#   ARUBA_CLIENT_ID=… ARUBA_CLIENT_SECRET=… ./seed-tenant.sh
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# Credentials can live in a gitignored .env next to this script instead of the
# environment, so they never reach shell history.
#   ARUBA_CLIENT_ID=...
#   ARUBA_CLIENT_SECRET=...
# shellcheck disable=SC1091
[ -f "$here/.env" ] && . "$here/.env"

# --- what to talk to -----------------------------------------------------------
KUBE_CONTEXT=${KUBE_CONTEXT:?set the kube context of the cluster running the stack}

# Deliberately NO default. install.sh defaults this to the original demo account;
# here the whole point is a different one, and silently seeding ARU-348095 again
# would be the wrong answer every time.
ARUBA_TENANT=${ARUBA_TENANT:?set the Aruba account to onboard, e.g. ARU-123456}

# --- the tenant's Aruba API client ----------------------------------------------
# One credential drives both halves: the operator authenticates to the CMP with it
# (via Vault, keyed by tenant), and Terraform mints its bearer token from it. It must
# belong to ARUBA_TENANT — the operator looks it up by that name and Aruba bills what
# it creates to that account.
ARUBA_CLIENT_ID=${ARUBA_CLIENT_ID:?required: the Aruba API client id for this tenant}
ARUBA_CLIENT_SECRET=${ARUBA_CLIENT_SECRET:?required: the Aruba API client secret for this tenant}
ARUBA_IDP=${ARUBA_IDP:-https://mylogin.aruba.it/auth}
ARUBA_REALM=${ARUBA_REALM:-cmp-new-apikey}
OIDC_REALM_URL="${ARUBA_IDP}/realms/${ARUBA_REALM}"

# --- must match the deployed stack ----------------------------------------------
# ARUBA_LOCATION is what the regional gateway stamps on resources; the SKU labels
# record the region as the gateway stamps it, so a mismatch makes the catalog
# invisible to its own tenant. Check with:
#   kubectl -n ecp-system get deploy ecp-gateway-regional \
#     -o jsonpath='{.spec.template.spec.containers[0].args}'
ARUBA_LOCATION=${ARUBA_LOCATION:-ITBG-Bergamo}
ARUBA_NS=${ARUBA_NS:-aruba-system}
VAULT_DEV_ROOT_TOKEN=${VAULT_DEV_ROOT_TOKEN:-root}

kc() { kubectl --context "$KUBE_CONTEXT" "$@"; }

echo "==> context $KUBE_CONTEXT | tenant $ARUBA_TENANT | location $ARUBA_LOCATION"

# --------------------------------------------------------------------------------
# 0. refuse to run against a cluster that has no stack
# --------------------------------------------------------------------------------
# Seeding a tenant into a cluster with no Vault and no gateways would "succeed" and
# leave a namespace nothing ever reconciles, so fail here with a clear reason instead.
kc -n "$ARUBA_NS" get pod vault-0 >/dev/null 2>&1 || {
    echo "no vault-0 in $ARUBA_NS: the stack is not deployed on this cluster (run install.sh first)" >&2
    exit 1
}

# --------------------------------------------------------------------------------
# 1. the subject the RoleAssignment grants to
# --------------------------------------------------------------------------------
# Roles never travel in the token, so the gateway needs a RoleAssignment naming the
# caller's `sub`. Mint a real token and read it off rather than guessing: for a
# client_credentials grant `sub` is the client's service-account user id, not its
# clientId. This doubles as a live check that the credentials work.
if [ -z "${JWT_SUBJECT:-}" ]; then
    token=$(curl -fsS -X POST "${OIDC_REALM_URL}/protocol/openid-connect/token" \
        -H 'Content-Type: application/x-www-form-urlencoded' \
        --data-urlencode grant_type=client_credentials \
        --data-urlencode "client_id=${ARUBA_CLIENT_ID}" \
        --data-urlencode "client_secret=${ARUBA_CLIENT_SECRET}" | jq -r .access_token)
    JWT_SUBJECT=$(echo "$token" | cut -d. -f2 | tr '_-' '/+' | base64 -d 2>/dev/null | jq -r .sub)
fi
[ -n "$JWT_SUBJECT" ] && [ "$JWT_SUBJECT" != null ] || { echo "could not resolve the token subject" >&2; exit 1; }
echo "==> token subject: $JWT_SUBJECT"

# --------------------------------------------------------------------------------
# 2. the tenant's credential, in Vault
# --------------------------------------------------------------------------------
# The operator runs in auth.mode=multi and looks a credential up per tenant, at
# <kvMount>/<tenant> with exactly these two keys — so this is what makes the account
# usable at all. The chart's Vault is dev mode and stores everything in memory, so a
# restarted vault-0 loses this and the script has to be re-run.
kc -n "$ARUBA_NS" exec vault-0 -- sh -c \
    "VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN='$VAULT_DEV_ROOT_TOKEN' \
     vault kv put kv/'$ARUBA_TENANT' client-id='$ARUBA_CLIENT_ID' client-secret='$ARUBA_CLIENT_SECRET'" >/dev/null
echo "==> vault seeded: kv/$ARUBA_TENANT"

# --------------------------------------------------------------------------------
# 3. the tenant's namespace, catalogs and RBAC
# --------------------------------------------------------------------------------
# tenant.yaml only — the Region CR in catalog.yaml is cluster-scoped and shared by
# every tenant, and re-pointing it from here would change the regional API URL for
# everyone. install.sh owns that file.
TENANT_NS=$(printf %s "$ARUBA_TENANT" | openssl dgst -sha3-224 | awk '{print $NF}')
sed -e "s|@TENANT@|${ARUBA_TENANT}|g" \
    -e "s|@TENANT_NS@|${TENANT_NS}|g" \
    -e "s|@ARUBA_LOCATION@|${ARUBA_LOCATION}|g" \
    -e "s|@SUBJECT@|${JWT_SUBJECT}|g" \
    "$here/tenant.yaml" | kc apply -f -

echo
echo "==> done. tenant namespace: $TENANT_NS"
echo "    drive it with: terraform apply -var tenant=$ARUBA_TENANT"
