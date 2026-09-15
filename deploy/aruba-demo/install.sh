#!/usr/bin/env bash
# Deploy the Aruba demo stack into ONE cluster:
#
#   aruba-system : arubacloud-resource-operator (the Aruba CMP backend)
#                  ecp-delegator, plugin=aruba  (SECA CR -> arubacloud.com CR)
#   ecp-system   : ecp gateways, global + regional, JWT auth against Aruba's IdP
#
# Everything is idempotent (helm upgrade --install / kubectl apply), so re-running
# it after changing a variable is the supported way to reconfigure.
#
#   KUBE_CONTEXT=kind-aruba-demo ARUBA_CLIENT_ID=… ARUBA_CLIENT_SECRET=… ./install.sh
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# Credentials can live in a gitignored .env next to this script instead of the
# environment, so they never reach shell history.
#   ARUBA_CLIENT_ID=...
#   ARUBA_CLIENT_SECRET=...
# shellcheck disable=SC1091
[ -f "$here/.env" ] && . "$here/.env"

# --- what to talk to -----------------------------------------------------------
KUBE_CONTEXT=${KUBE_CONTEXT:?set the kube context to deploy into}
ARUBA_TENANT=${ARUBA_TENANT:-ARU-348095}

# Two different names for one place, and they are NOT interchangeable.
#
# REGION is the SECA region: the name of the Region CR, so it has to be a legal
# Kubernetes object name — lowercase. It is what a client asks for and what the
# region catalog is keyed by.
#
# ARUBA_LOCATION is what the regional gateway STAMPS on every resource it writes
# (`--region`, frozen into the config singleton at startup), and every aruba
# converter forwards that string to the CMP verbatim as the location. Aruba's
# location is spelled "ITBG-Bergamo" and is not case-insensitive:
#
#   Validation: Name: Location Value: itbg-bergamo not found.
#
# The workspace/block-storage demo never hit this — the Project converter sets no
# region at all, and a block storage takes it from its (region-less) refs and falls
# through to the plugin's own "ITBG-Bergamo" default. Everything else — VPC, subnet,
# elastic IP, security group, key pair, CloudServer — forwards the stamped value.
REGION=${REGION:-itbg-bergamo}
ARUBA_LOCATION=${ARUBA_LOCATION:-ITBG-Bergamo}

# --- Aruba production API client ------------------------------------------------
# The operator authenticates to the Aruba CMP with this; Terraform mints its bearer
# token from the same client, so one credential drives both halves.
ARUBA_CLIENT_ID=${ARUBA_CLIENT_ID:?required: the Aruba API client id}
ARUBA_CLIENT_SECRET=${ARUBA_CLIENT_SECRET:?required: the Aruba API client secret}
ARUBA_API_GATEWAY=${ARUBA_API_GATEWAY:-https://api.arubacloud.com}
ARUBA_IDP=${ARUBA_IDP:-https://mylogin.aruba.it/auth}
ARUBA_REALM=${ARUBA_REALM:-cmp-new-apikey}
OIDC_REALM_URL="${ARUBA_IDP}/realms/${ARUBA_REALM}"

# --- versions -------------------------------------------------------------------
ECP_VERSION=${ECP_VERSION:-0.0.3-alpha}
OPERATOR_VERSION=${OPERATOR_VERSION:-1.0.1}

# --- the chart-installed Vault ---------------------------------------------------
# config.auth.multi.vault.auto.devRootToken, left at the chart default. Dev mode, so
# this token is the whole of Vault's security: keep the service ClusterIP-only.
VAULT_DEV_ROOT_TOKEN=${VAULT_DEV_ROOT_TOKEN:-root}

# --- gateway image override ------------------------------------------------------
# Normally unset: the chart's image tag follows ECP_VERSION. Point these at a locally
# built image only to test a gateway change before it is released.
#
# ECP_VERSION must be >= 0.0.3-alpha. v0.0.2-alpha CANNOT authenticate a real OIDC
# token: it decodes the "scope" claim as a SECA down-scope object, and every
# standards-compliant issuer stamps it as the OAuth 2.0 space-delimited string
# ("scope":"email" from Keycloak), so the whole token fails to decode and every
# request is a 401.
GATEWAY_IMAGE_REPO_GLOBAL=${GATEWAY_IMAGE_REPO_GLOBAL:-}
GATEWAY_IMAGE_REPO_REGIONAL=${GATEWAY_IMAGE_REPO_REGIONAL:-}
GATEWAY_IMAGE_TAG=${GATEWAY_IMAGE_TAG:-}

# --- namespaces -----------------------------------------------------------------
ECP_NS=${ECP_NS:-ecp-system}
ARUBA_NS=${ARUBA_NS:-aruba-system}

# --- private GHCR ---------------------------------------------------------------
# ghcr.io/eu-sovereign-cloud/ecp/* packages are private, so every pod that runs an
# ECP image needs a pull secret. A gh CLI token with read:packages works.
GHCR_USER=${GHCR_USER:-$(gh api user -q .login)}
GHCR_TOKEN=${GHCR_TOKEN:-$(gh auth token)}
PULL_SECRET=${PULL_SECRET:-ghcr-ecp}

# --- what the region catalog advertises -----------------------------------------
# A SECA client resolves the regional API from the Region CR, so this address must
# be reachable from wherever Terraform runs. With ClusterIP services that is the
# local end of `kubectl port-forward svc/ecp-gateway-regional 3001:80`.
REGIONAL_URL=${REGIONAL_URL:-http://127.0.0.1:3001}

kc() { kubectl --context "$KUBE_CONTEXT" "$@"; }
hc() { helm --kube-context "$KUBE_CONTEXT" "$@"; }

echo "==> context $KUBE_CONTEXT | tenant $ARUBA_TENANT | region $REGION ($ARUBA_LOCATION)"

# --------------------------------------------------------------------------------
# 1. namespaces + image pull secret
# --------------------------------------------------------------------------------
for ns in "$ECP_NS" "$ARUBA_NS"; do
    kc create namespace "$ns" --dry-run=client -o yaml | kc apply -f -
    kc -n "$ns" create secret docker-registry "$PULL_SECRET" \
        --docker-server=ghcr.io \
        --docker-username="$GHCR_USER" \
        --docker-password="$GHCR_TOKEN" \
        --dry-run=client -o yaml | kc apply -f -
done

# --------------------------------------------------------------------------------
# 2. the JWT verification key, straight from the realm's JWKS
# --------------------------------------------------------------------------------
# The gateway verifies offline against one static PEM public key — it does not
# fetch a JWKS — so the realm's current RS256 signing key is materialised here.
# Re-run this script after a realm key rotation.
jwt_pub="$(mktemp)"; trap 'rm -f "$jwt_pub"' EXIT
curl -fsS "${OIDC_REALM_URL}/protocol/openid-connect/certs" \
  | jq -r '.keys[] | select(.use=="sig" and .alg=="RS256") | .x5c[0]' \
  | { echo "-----BEGIN CERTIFICATE-----"; fold -w 64; echo "-----END CERTIFICATE-----"; } \
  | openssl x509 -pubkey -noout > "$jwt_pub"
echo "==> RS256 verification key fetched from ${OIDC_REALM_URL}"

# --------------------------------------------------------------------------------
# 3. the subject the RoleAssignment grants to
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
# 4. the Aruba backend: arubacloud-resource-operator
# --------------------------------------------------------------------------------
# auth.mode=multi reads one credential per tenant out of Vault instead of taking a
# single cluster-wide API client. setup=auto has the chart install Vault itself (dev
# mode, in-memory) and configure the AppRole for it: a post-install/post-upgrade Job
# enables approle + a kv-v2 mount, writes a read-only policy for kv/data/*, and
# patches the role-id/secret-id it minted into the operator-config Secret. mode,
# setup and vault.enabled have to agree or the chart's own `operator.validate`
# fails the render.
#
# NO --wait HERE. Helm waits for the release's resources *before* running its
# post-upgrade hooks, and the operator Deployment gained an initContainer that
# blocks until role-id is populated — which is exactly what that hook does. --wait
# deadlocks the two against each other until the timeout. Hooks are waited on
# regardless, so when this returns the Job has already run.
#
# --force-conflicts is what makes a SECOND run possible at all. Helm 4 applies
# server-side, and the chart's own vault-config Job writes the credentials with
# `kubectl patch` — so `kubectl-patch` ends up owning .data.role-id/.secret-id while
# the template still renders them as "". Every later upgrade is then a field-manager
# conflict and Helm refuses:
#   Apply failed with 2 conflicts: conflicts with "kubectl-patch"
# Forcing is safe precisely because the Job runs again afterwards and re-patches
# both fields; the operator pod is untouched in between, having read its config at
# startup.
hc repo add arubacloud https://arubacloud.github.io/helm-charts --force-update >/dev/null
hc upgrade --install arubacloud-resource-operator arubacloud/arubacloud-resource-operator \
    --version "$OPERATOR_VERSION" --namespace "$ARUBA_NS" --force-conflicts \
    --set config.gateway="$ARUBA_API_GATEWAY" \
    --set config.auth.idp="$ARUBA_IDP" \
    --set config.auth.realm="$ARUBA_REALM" \
    --set config.auth.mode=multi \
    --set config.auth.multi.setup=auto \
    --set vault.enabled=true \
    --timeout 10m

# The tenant credential the operator will look up. The SDK reads it from
# <kvMount>/<tenant> — kvPrefix is empty — and wants exactly these two keys; the
# tenant is the `spec.tenant` the aruba plugin stamps on every arubacloud.com CR,
# i.e. ARUBA_TENANT. Seeding is part of the install because the chart's Vault is
# dev mode: storage is in memory, so a restarted vault-0 comes back with no
# AppRole and no credentials and this script is what puts them back.
# `rollout status` does not work here: the Vault chart's StatefulSet uses the
# OnDelete update strategy, which rollout status refuses outright.
kc -n "$ARUBA_NS" wait --for=condition=Ready pod/vault-0 --timeout=5m >/dev/null
kc -n "$ARUBA_NS" exec vault-0 -- sh -c \
    "VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN='$VAULT_DEV_ROOT_TOKEN' \
     vault kv put kv/'$ARUBA_TENANT' client-id='$ARUBA_CLIENT_ID' client-secret='$ARUBA_CLIENT_SECRET'" >/dev/null
echo "==> vault seeded: kv/$ARUBA_TENANT"
kc -n "$ARUBA_NS" rollout status deploy/arubacloud-resource-operator-controller --timeout=5m

# --------------------------------------------------------------------------------
# 5. the ECP gateways
# --------------------------------------------------------------------------------
# The chart carries the SECA CRDs, but Helm installs them only on first install and
# never upgrades them — so on a version bump the cluster would silently keep the old
# schemas. Apply the release's crds/ first; it is a no-op on a fresh cluster and the
# only thing that makes an upgrade correct.
hc repo add ecp https://eu-sovereign-cloud.github.io/ecp/ --force-update >/dev/null
chart_dir=$(mktemp -d); trap 'rm -f "$jwt_pub"; rm -rf "$chart_dir"' EXIT
hc pull ecp/ecp --version "$ECP_VERSION" --untar --untardir "$chart_dir" >/dev/null
kc apply -f "$chart_dir/ecp/crds/"
gateway_image_args=()
if [ -n "$GATEWAY_IMAGE_REPO_GLOBAL" ]; then
    gateway_image_args+=(
        --set gatewayGlobal.image.repository="$GATEWAY_IMAGE_REPO_GLOBAL"
        --set gatewayRegional.image.repository="$GATEWAY_IMAGE_REPO_REGIONAL"
        --set gatewayGlobal.image.tag="$GATEWAY_IMAGE_TAG"
        --set gatewayRegional.image.tag="$GATEWAY_IMAGE_TAG"
    )
fi
hc upgrade --install ecp ecp/ecp --version "$ECP_VERSION" --namespace "$ECP_NS" \
    --values "$here/values-ecp.yaml" \
    ${gateway_image_args[@]+"${gateway_image_args[@]}"} \
    --set gatewayRegional.region="$ARUBA_LOCATION" \
    --set-file auth.jwt.key="$jwt_pub" \
    --set imagePullSecrets[0].name="$PULL_SECRET" \
    --wait --timeout 5m

# --------------------------------------------------------------------------------
# 6. the aruba delegator
# --------------------------------------------------------------------------------
# plugin=aruba selects both the delegator-aruba image and the RBAC for exactly the
# controllers that plugin loads — including the arubacloud.com CRs it writes.
hc upgrade --install ecp-delegator ecp/ecp-delegator --version "$ECP_VERSION" \
    --namespace "$ARUBA_NS" \
    --set plugin=aruba \
    --set imagePullSecrets[0].name="$PULL_SECRET" \
    --wait --timeout 5m

# --------------------------------------------------------------------------------
# 7. the catalog and the tenant's RBAC
# --------------------------------------------------------------------------------
# Two files, split by blast radius. catalog.yaml is the cluster-scoped Region, shared
# by every tenant on the cluster; tenant.yaml is scoped to one tenant and is all that
# seed-tenant.sh applies when a second account is onboarded.
TENANT_NS=$(printf %s "$ARUBA_TENANT" | openssl dgst -sha3-224 | awk '{print $NF}')
sed -e "s|@REGION@|${REGION}|g" \
    -e "s|@REGIONAL_URL@|${REGIONAL_URL}|g" \
    "$here/catalog.yaml" | kc apply -f -
sed -e "s|@TENANT@|${ARUBA_TENANT}|g" \
    -e "s|@TENANT_NS@|${TENANT_NS}|g" \
    -e "s|@ARUBA_LOCATION@|${ARUBA_LOCATION}|g" \
    -e "s|@SUBJECT@|${JWT_SUBJECT}|g" \
    "$here/tenant.yaml" | kc apply -f -

# The gateways read the JWT key file once, at startup, and the chart puts no checksum
# annotation on the pod template — so a re-run that fetched a rotated realm key would
# update the Secret and change nothing. Restart unconditionally; it is one rollout.
kc -n "$ECP_NS" rollout restart deploy/ecp-gateway-global deploy/ecp-gateway-regional >/dev/null
kc -n "$ECP_NS" rollout status deploy/ecp-gateway-global --timeout=3m
kc -n "$ECP_NS" rollout status deploy/ecp-gateway-regional --timeout=3m

echo
echo "==> done. tenant namespace: $TENANT_NS"
kc -n "$ECP_NS" get deploy
kc -n "$ARUBA_NS" get deploy
