# Aruba demo stack

One cluster running the whole path from a Terraform `apply` to a real volume in
the Aruba CMP, with the SECA API protected by JWTs minted by Aruba's production
IdP.

```
terraform  ──JWT──►  ecp-gateway-global    (region catalog, RBAC)      ecp-system
                 └─►  ecp-gateway-regional  (workspace/storage/…)      ecp-system
                          │ writes SECA CRs
                          ▼
                      ecp-delegator (plugin=aruba)                     aruba-system
                          │ writes arubacloud.com CRs
                          ▼
                      arubacloud-resource-operator ──► api.arubacloud.com
```

| Component | Namespace | Source |
|---|---|---|
| `arubacloud-resource-operator` | `aruba-system` | `arubacloud/arubacloud-resource-operator` 1.0.1 |
| `ecp-delegator` (plugin `aruba`) | `aruba-system` | `ecp/ecp-delegator` 0.0.3-alpha |
| `ecp-gateway-global`, `ecp-gateway-regional` | `ecp-system` | `ecp/ecp` 0.0.3-alpha |

Services are `ClusterIP`; everything is reached with `kubectl port-forward`.

## There is also a Go CLI

[`client-cli/`](client-cli) is a single binary that does everything below **and**
creates the Kubernetes cluster itself, as an Aruba KaaS cluster, instead of
assuming a `kind` or a hand-built one. It runs the operator in `auth.mode=single`,
so it drops the Vault entirely — see [its README](client-cli/README.md), which
also answers whether `ARUBA_TENANT` is still needed (short version: not by the
operator, still by SECA).

The scripts below remain the multi-tenant path.

## Prerequisites

- `kubectl`, `helm`, `jq`, `openssl`, `curl`, and `gh` logged in with the
  `read:packages` scope (`gh auth refresh -h github.com -s read:packages`).
- An Aruba API client (`clientId` / `clientSecret`) for the account in
  `ARUBA_TENANT`. The same client is used twice: the operator authenticates to
  the CMP with it, and Terraform mints its bearer token from it.
- `terraform >= 1.13` for [`../../tf`](../../tf).

> ⚠️ This provisions **real, billable** Aruba resources against `ARUBA_TENANT`.

## Install

```shell
export ARUBA_CLIENT_ID=<client-id>
export ARUBA_CLIENT_SECRET=<client-secret>

# 1. a throwaway cluster to rehearse on
kind create cluster --name aruba-demo
KUBE_CONTEXT=kind-aruba-demo ./install.sh

# 2. the real one — same script, different context
KUBE_CONTEXT=<your-cluster-context> ./install.sh
```

`install.sh` is idempotent (`helm upgrade --install` / `kubectl apply`), so
re-running it is how you reconfigure. What it does, in order:

1. Creates `ecp-system` and `aruba-system`, and an image-pull Secret in each —
   the `ghcr.io/eu-sovereign-cloud/ecp/*` packages are **private**, so without it
   every ECP pod sits in `ImagePullBackOff`. The credential is a `gh` CLI token;
   it expires, so a cluster that stops pulling needs the script re-run.
2. Fetches the realm's RS256 signing key from the JWKS endpoint and converts the
   `x5c` certificate to a PEM public key.
3. Mints a token with the client credentials and reads its `sub` claim. This is
   both a live credential check and where the RoleAssignment's subject comes
   from — for a `client_credentials` grant `sub` is the client's service-account
   user id, **not** the `clientId`, so it cannot be guessed.
4. Installs the `arubacloud-resource-operator` pointed at production
   (`https://api.arubacloud.com`, `https://mylogin.aruba.it/auth`, realm
   `cmp-new-apikey`) in `auth.mode=multi` with `multi.setup=auto`, which also
   installs a dev-mode Vault as a sub-chart and configures its AppRole, then
   writes the API client into `kv/$ARUBA_TENANT` — see
   [Where the credentials live](#where-the-credentials-live).
5. Installs the `ecp` chart (both gateways, JWT auth) — this also installs the
   SECA CRDs.
6. Installs the `ecp-delegator` chart with `plugin=aruba`, which selects both the
   `delegator-aruba` image and RBAC for exactly the controllers that plugin loads.
7. Applies [`catalog.yaml`](catalog.yaml) — the cluster-scoped Region CR — and
   [`tenant.yaml`](tenant.yaml) — the tenant namespace, the StorageSKU /
   InstanceSKU / NetworkSKU catalogs, and the Role + RoleAssignment that authorize
   the token subject. They are two files because they have two blast radii; see
   [Adding a second tenant](#adding-a-second-tenant).

Variables (all optional except the credentials) are at the top of
[`install.sh`](install.sh); the ones worth knowing:

| Variable | Default | Why you would change it |
|---|---|---|
| `KUBE_CONTEXT` | _(required)_ | Which cluster. Every command in the script is explicit about it. |
| `ARUBA_TENANT` | `ARU-348095` | The Aruba account. Also the SECA tenant, and what the tenant namespace hashes from. |
| `REGION` | `itbg-bergamo` | Name of the Region CR. Lowercase, because it is a Kubernetes object name. |
| `ARUBA_LOCATION` | `ITBG-Bergamo` | What the regional gateway stamps on resources, and what Aruba is told the location is. See below. |
| `REGIONAL_URL` | `http://127.0.0.1:3001` | What the region catalog advertises — see below. |
| `JWT_SUBJECT` | _(derived)_ | Set it to skip minting a token, e.g. to rehearse with placeholder credentials. |
| `VAULT_DEV_ROOT_TOKEN` | `root` | The chart's `devRootToken`. Change it in both places or the seeding step cannot log in. |

### Why `REGIONAL_URL` is a localhost address

A SECA client is given only the **global** endpoint. It resolves the regional API
(workspace, storage, network, compute) by reading the provider URLs off the
Region CR — so those URLs have to be reachable **from the client**, not from
inside the cluster. With `ClusterIP` services the client reaches the regional
gateway through a port-forward, so that is what the catalog has to advertise. If
you later expose the gateways through an Ingress or a LoadBalancer, set
`REGIONAL_URL` to the external address and re-run.

### The region has two names

`REGION` and `ARUBA_LOCATION` look redundant and are not.

`REGION` is the SECA region — the `Region` CR's name, so it has to be a legal
Kubernetes object name, which means lowercase. It is what a client asks for and
what the region catalog is keyed by.

`ARUBA_LOCATION` is what the regional gateway **stamps** on every resource it
writes (`--region`, frozen into the config singleton at startup), and every aruba
converter forwards that string to the CMP verbatim as the location. Aruba spells
it `ITBG-Bergamo` and does not accept another case:

```
Validation: Name: Location Value: itbg-bergamo not found.
```

The workspace + block-storage half of the demo never hit this — a Project carries
no region at all and a block storage falls through to the plugin's own
`ITBG-Bergamo` default — so it only surfaced once the stack created a VPC. The SKU
catalogs are labelled with `ARUBA_LOCATION` too, because that label records the
region as the gateway stamps it.

## Adding a second tenant

The operator runs in `auth.mode=multi` and resolves a credential per tenant out of
Vault, so one cluster serves several Aruba accounts. Onboarding one does **not**
need `install.sh` — [`seed-tenant.sh`](seed-tenant.sh) is the onboarding half of it
and nothing else:

```shell
KUBE_CONTEXT=… ARUBA_TENANT=ARU-123456 \
ARUBA_CLIENT_ID=… ARUBA_CLIENT_SECRET=… ./seed-tenant.sh
```

It writes exactly two things — the tenant's credential at `kv/<tenant>`, and
[`tenant.yaml`](tenant.yaml): the tenant's namespace, SKU catalogs, Role and
RoleAssignment. No helm, no gateways, no operator, no delegator.

That is also why the catalog is split in two. Everything in `tenant.yaml` is scoped
to one tenant and lands in `sha3-224("<tenant>")`, so applying it can never disturb
an account that is already working. `catalog.yaml` holds the one cluster-scoped
object — the `Region` CR every tenant shares — and only `install.sh` applies it,
because re-applying it with a different `REGIONAL_URL` would re-point the regional
API for everyone at once.

`ARUBA_LOCATION` has to match the running stack, since the SKU labels record the
region as the gateway stamps it and a mismatch makes the catalog invisible to its
own tenant. Check it with:

```shell
kubectl -n ecp-system get deploy ecp-gateway-regional \
  -o jsonpath='{.spec.template.spec.containers[0].args}'
```

Then drive the new tenant with `terraform apply -var tenant=ARU-123456`. Resource
names do not have to be unique across tenants: they are separated by the tenant
namespace in the cluster and by the account in Aruba.

> The script needs `exec` on `vault-0`, which is effectively root on the dev-mode
> Vault — the root token is `root`. Run it yourself for someone rather than handing
> that access out, unless they are meant to have it.

## Use it

```shell
kubectl -n ecp-system port-forward svc/ecp-gateway-global   3000:80 &
kubectl -n ecp-system port-forward svc/ecp-gateway-regional 3001:80 &

TOKEN=$(curl -s -X POST \
  https://mylogin.aruba.it/auth/realms/cmp-new-apikey/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  --data-urlencode "client_id=$ARUBA_CLIENT_ID" \
  --data-urlencode "client_secret=$ARUBA_CLIENT_SECRET" | jq -r .access_token)

curl -s -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/providers/seca.region/v1/regions | jq
curl -s -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3001/providers/seca.storage/v1/tenants/ARU-348095/skus | jq
```

Then drive it from Terraform: [`../../tf`](../../tf).

### Checking that auth is actually on

| Request | Expected |
|---|---|
| no `Authorization` header | `401` |
| `Authorization: Bearer garbage` | `401` |
| a JWT signed by any other key | `401` |
| a valid Aruba JWT whose `sub` has no RoleAssignment | `403` on RBAC-governed providers, `200` on `seca.region` |
| the granted subject | `200` |

`seca.region` is served **authn-only** (`--authz-skip-providers=seca.region`):
the region resource is tenant-less by spec, so there is no tenant namespace to
read policy from. A valid token is still required.

## What the auth model actually is

The gateway verifies **offline** against one static PEM public key — it does not
fetch a JWKS. So the trust chain is:

- **signature** — only tokens signed by the `cmp-new-apikey` realm's current
  RS256 key verify. A realm key rotation breaks every token until you re-run
  `install.sh` (see the caveat below about restarting the pods).
- **`exp`** — mandatory, so a token cannot opt out of expiring. The realm mints
  1-hour tokens (`expires_in: 3600`). Verification is offline, so a token stays
  valid for that full hour whatever happens to the account behind it: disabling
  the API client in the Aruba console does **not** cut off a token already
  issued. Removing the RoleAssignment is immediate by comparison — that is the
  lever to pull if a token leaks.
- **`sub` → RoleAssignment** — roles never travel in the token. Every
  entitlement comes from the `Role`/`RoleAssignment` pair in the tenant
  namespace, which is why `tenant.yaml` has to name the subject.

`--jwt-issuer` / `--jwt-audience` are **not** set: the chart gained
`auth.jwt.issuer` / `auth.jwt.audience` after v0.0.2-alpha, and this deployment
does not pass them. It costs nothing here — a Keycloak realm's signing key signs
for that realm alone, so `iss` could only ever be
`https://mylogin.aruba.it/auth/realms/cmp-new-apikey`, and Aruba's token carries
no `aud` at all. Turn them on if a second issuer ever shares the key.

## Where the credentials live

The operator runs in `auth.mode=multi`: it does not hold one cluster-wide API
client, it looks a credential up **per tenant** in Vault, on every tenant it has
not seen before. The path is not configurable per resource — the SDK builds it
as `<kvMount>/<kvPrefix>/<tenant>`, `kvPrefix` is empty here, and `<tenant>` is
the `spec.tenant` the aruba plugin stamps on every `arubacloud.com` CR, which is
`ARUBA_TENANT`. So exactly one secret matters, and it must carry exactly these
two keys:

```shell
kubectl -n aruba-system exec vault-0 -- sh -c \
  'VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root \
   vault kv put kv/ARU-348095 client-id="<id>" client-secret="<secret>"'
```

`install.sh` does this for you from `ARUBA_CLIENT_ID` / `ARUBA_CLIENT_SECRET`.

The operator reaches Vault with an AppRole the chart's `vault-config` Job creates
— `operator-policy` grants `read` on `kv/data/*` and nothing else — and the
role-id/secret-id it mints are patched into the `operator-config` Secret. The
operator reads that Secret **once, at startup**, so a re-issued secret-id only
takes effect on the next pod restart. The old one keeps working in the meantime:
the role is created with `secret_id_ttl=0` and `secret_id_num_uses=0`.

Nothing here is production-grade. Vault runs in dev mode behind the root token
`root`, in memory, on a `ClusterIP` service — the isolation is that nothing
outside the cluster can reach it. For anything real, use `setup=manual` against a
Vault you actually operate.

### What a missing credential looks like

The operator builds its Aruba client lazily, on the first reconcile of a resource
for that tenant, and a failure there is returned as a plain error — controller-
runtime requeues with backoff and the resource's phase is **not** touched. So an
empty Vault does not mark anything `Failed`; existing resources simply stop being
reconciled and the log fills with client-creation errors. Seed the secret and it
recovers on the next backoff tick, with no restart needed.

## Two bugs this shook out

Both were found by pointing the stack at a real IdP and a real backend; neither
shows up against the test fixtures.

### 1. The gateway could not authenticate any real OIDC token

`ecp-gateway` v0.0.2-alpha decoded the JWT `scope` claim straight into the SECA
down-scope object. OAuth 2.0 defines `scope` as a space-delimited **string**
(RFC 6749 §3.3, RFC 9068 §2.2.3) and every OIDC issuer stamps it — Aruba's
Keycloak sends `"scope":"email"` on a bare `client_credentials` grant. The claim
set failed to decode, so the *whole token* was rejected and every request came
back `401` with no indication why:

```
token is malformed: could not JSON decode claim:
json: cannot unmarshal string into Go struct field jwtClaims.scope of type resource.TokenScope
```

Fixed in `gateway/internal/authn/jwtstd.go` (a string is decoded and ignored — it
caps nothing, exactly like omitting the claim) and released as **v0.0.3-alpha**.
`ECP_VERSION` defaults to it; v0.0.2-alpha cannot do JWT auth against a real
issuer at all.

### 2. The SDK's polling loop backs *off* backwards

`secapi`'s resource observer builds an `ExponentialBackOff` with
`Multiplier = 0.5` — below 1, so each wait is **half** the previous one. The
series converges to roughly `2 × interval` no matter how large `max_attempts`
is, so `delay=30, interval=10, max_attempts=30` gives up about 50s in rather
than the ~330s it reads as. A 20 GB volume takes ~65s, so Terraform failed with
`max retry attempts reached` on a resource that then went active seconds later.

The `retry` block in [`../../tf/main.tf`](../../tf/main.tf) is tuned around this:
`delay` sets when the first check happens and `interval` sets the whole remaining
budget. This is a `go-sdk` bug and the Terraform provider vendors its own copy,
so it cannot be fixed from this repository.

## Gotchas found the hard way

- **`--wait` on the operator deadlocks the chart against itself.** Helm waits
  for a release's resources *before* it runs the post-install/post-upgrade
  hooks. In `setup=auto` the operator Deployment carries an initContainer that
  blocks until `role-id` is populated in the `operator-config` Secret — and the
  only thing that populates it is the `vault-config` Job, which is one of those
  hooks. Each waits for the other until the timeout. Drop `--wait`; hooks are
  waited on regardless, so the Job has still run by the time Helm returns.
- **The second `helm upgrade` fails with a field-manager conflict.** Helm 4
  applies server-side, and that same Job writes the credentials with `kubectl
  patch`, so `kubectl-patch` ends up owning `.data.role-id` / `.data.secret-id`
  while the template still renders them as `""`. The next upgrade is a conflict
  and Helm refuses:
  `Apply failed with 2 conflicts: conflicts with "kubectl-patch"`.
  `--force-conflicts` is the fix and is safe here — the Job re-patches both
  fields immediately afterwards, and the running operator pod read its config at
  startup so it is unaffected in between.
- **The chart's Vault is dev mode: everything is in memory.** `vault.server.dev`
  with `dataStorage.enabled=false`. A restarted `vault-0` comes back with no
  AppRole, no KV mount and no tenant credential, and the role-id/secret-id in the
  `operator-config` Secret are stale — resources stop reconciling. Re-running
  `install.sh` repairs all of it: the hook rebuilds the AppRole and re-patches the
  Secret, and the script re-seeds the KV. Restart the operator afterwards so it
  picks up the new secret-id:
  `kubectl -n aruba-system rollout restart deploy/arubacloud-resource-operator-controller`.
- **`kubectl rollout status` does not work on the Vault StatefulSet.** Its update
  strategy is `OnDelete`, which `rollout status` refuses outright
  (`rollout status is only available for RollingUpdate strategy type`). Wait on
  `pod/vault-0` being `Ready` instead.
- **Rotating the JWT key needs a pod restart.** The key is a file mounted from a
  Secret and read once at startup, and the chart puts no checksum annotation on
  the pod template — so `helm upgrade` with a new `auth.jwt.key` updates the
  Secret and changes nothing until:
  `kubectl -n ecp-system rollout restart deploy/ecp-gateway-global deploy/ecp-gateway-regional`.
- **Helm never upgrades CRDs it installed.** On an ECP version bump, apply the
  new chart's `crds/` by hand.
- **A bad `client_id`/`client_secret` is silent in the operator.** Nothing fails
  at install; resources simply stay `Pending` and the operator logs
  `authentication failed: oauth2: "invalid_client"`. `install.sh` mints a token
  up front so this is caught before anything is deployed.

## Teardown

Delete the SECA resources **before** the cluster. Their finalizers are what make
the plugin delete the `arubacloud.com` CRs, which is what makes the operator reap
what it created in the CMP — dropping the cluster first strands real, billable
resources.

```shell
terraform -chdir=../../tf destroy      # or: kubectl delete the SECA CRs
helm -n ecp-system   uninstall ecp
helm -n aruba-system uninstall ecp-delegator arubacloud-resource-operator
kind delete cluster --name aruba-demo  # only for the rehearsal cluster
```
