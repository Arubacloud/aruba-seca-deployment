# `aruba-demo` — the demo stack as one binary

The Go replacement for [`../install.sh`](../install.sh) and
[`../seed-tenant.sh`](../seed-tenant.sh), plus the one thing the scripts could
not do: **it builds the Kubernetes cluster too**, as a real Aruba KaaS cluster,
instead of assuming a `kind` cluster or a hand-built one.

```
aruba-demo up
   │
   ├─ Aruba Cloud API (github.com/Arubacloud/sdk-go)
   │     Project ──► VPC ──► Subnet ──► KaaS cluster ──► kubeconfig
   │
   └─ that cluster, through the downloaded kubeconfig FILE
         ├─ arubacloud-resource-operator   (auth.mode=single)   aruba-system
         ├─ ecp-delegator (plugin=aruba)                        aruba-system
         ├─ ecp-gateway-global / -regional (JWT auth)           ecp-system
         ├─ Region CR (the region catalog)                      cluster-scoped
         └─ tenant namespace: SKUs + Role + RoleAssignment      sha3-224(tenant)
```

It never reads or switches your ambient kube context. Every cluster call goes
through the kubeconfig file named by `--kubeconfig`, which defaults to
`./<cluster>.kubeconfig` — the file the tool downloads from the KaaS cluster.

> ⚠️ `cluster create` and `up` provision **real, billable** Aruba resources.

## Build

```shell
cd deploy/aruba-demo/client-cli
go build -o aruba-demo .
```

## Quick start

```shell
export ARUBA_CLIENT_ID=<client-id>
export ARUBA_CLIENT_SECRET=<client-secret>

./aruba-demo up --tenant ARU-348095
```

That is: create the cluster (10–20 minutes), download its kubeconfig, then
install the whole stack. Then reach the gateways the usual way:

```shell
export KUBECONFIG=$PWD/ecp-demo.kubeconfig
kubectl -n ecp-system port-forward svc/ecp-gateway-global   3000:80 &
kubectl -n ecp-system port-forward svc/ecp-gateway-regional 3001:80 &
```

and drive it from Terraform: [`../../../tf`](../../../tf).

## Credentials

Resolved in this order, first hit wins:

1. `--client-id` / `--client-secret`
2. `ARUBA_CLIENT_ID` / `ARUBA_CLIENT_SECRET`
3. a `.env` file — `--env-file`, else `./.env`, else `../.env`, which is the
   same gitignored file the shell scripts source
4. an interactive prompt (the secret is read with echo off)

One credential drives both halves: the operator authenticates to the CMP with
it, and Terraform mints its bearer token from it.

The **image pull** credential is separate. The `ghcr.io/eu-sovereign-cloud/ecp/*`
packages are private, so a Secret is written into both namespaces or every ECP
pod sits in `ImagePullBackOff`. It defaults to the `gh` CLI's own token
(`gh api user -q .login` / `gh auth token`), which needs the `read:packages`
scope — `gh auth refresh -h github.com -s read:packages`. Override with
`--ghcr-user` / `--ghcr-token`. gh tokens expire, so a cluster that stops
pulling needs `install` re-run.

## Commands

Every command is idempotent. `install` is `helm upgrade --install` plus
server-side apply throughout; the `cluster` commands look every resource up by
name before creating it, so a re-run adopts what is already there rather than
billing a second copy.

### `up`

`cluster create` followed by `install`, in one shot. Takes every flag both take.

### `cluster create`

Creates or adopts, in order: the **Project**, a **VPC** (`<cluster>-vpc`), an
advanced **Subnet** (`<cluster>-subnet`), and the **KaaS cluster**, then writes
its kubeconfig. The KaaS API stores only a security group's *name* and creates
the group itself, so nothing has to exist beforehand.

| Flag | Default | Notes |
|---|---|---|
| `--cluster` | `ecp-demo` | names the cluster, its VPC, its subnet and its node pool |
| `--project` | `ecp-demo` | Aruba project; created if absent |
| `--k8s-version` | `1.34.1` | one Aruba publishes: `1.32.3`, `1.33.2`, `1.34.1` |
| `--node-flavor` | `K4A8` | 4 vCPU / 8 GB. `K1A2`, `K2A4`, `K8A16`, … |
| `--nodes` | `2` | node pool size |
| `--zone` | `ITBG-1` | availability zone |
| `--subnet-cidr` | `10.0.1.0/24` | |
| `--pod-cidr` | `10.0.3.0/24` | |
| `--node-cidr` | `172.16.0.0/16` | |
| `--ha` | `false` | highly available control plane; costs more |
| `--kubeconfig` | `./<cluster>.kubeconfig` | written `0600` |

The cluster takes 10–20 minutes to converge. The wait budget is 40 minutes.

### `cluster kubeconfig`

Re-downloads the kubeconfig for an existing cluster. Takes `--project` and
`--cluster`.

### `cluster delete`

Deletes the KaaS cluster, then its subnet, then its VPC. **The project is left
alone** — it may be shared, and Aruba refuses a non-empty one.

> Delete the SECA resources **first** (`terraform destroy`, or delete the CRs).
> Their finalizers are what make the plugin delete the `arubacloud.com` CRs,
> which is what makes the operator reap what it created in the CMP. Dropping the
> cluster first strands real, billable resources.

Asks for confirmation unless `--yes` is passed.

### `install`

The `install.sh` replica. In order:

1. The two namespaces and the GHCR pull Secret in each.
2. The realm's current RS256 signing key, fetched from its JWKS and converted
   from the `x5c` certificate to a PEM **public key**. The gateway verifies
   offline against this one static key — it does not fetch a JWKS — so a realm
   key rotation needs `install` re-run.
3. A `client_credentials` token, minted only to read its `sub` claim. This is
   both a live credential check and where the RoleAssignment's subject comes
   from: for a `client_credentials` grant `sub` is the client's *service account
   user id*, not the `clientId`, so it cannot be guessed. `--jwt-subject` skips
   the minting and grants that subject instead.
4. `arubacloud-resource-operator` in `auth.mode=single` — see
   [Why there is no Vault](#why-there-is-no-vault).
5. The `ecp` chart's `crds/`, applied by hand first (Helm installs a chart's
   CRDs only on a *first* install and never upgrades them), then the chart
   itself: both gateways, JWT auth, RBAC.
6. `ecp-delegator` with `plugin=aruba`, which selects both the `delegator-aruba`
   image and the RBAC for exactly the controllers that plugin loads.
7. [`catalog.yaml`](manifests/catalog.yaml) — the cluster-scoped Region CR — and
   [`tenant.yaml`](manifests/tenant.yaml) — the tenant namespace, the SKU
   catalogs, and the Role + RoleAssignment.
8. A rollout restart of both gateways. They read the JWT key file once at
   startup and the chart puts no checksum annotation on the pod template, so a
   re-run that fetched a rotated key would update the Secret and change nothing.

| Flag | Default | Notes |
|---|---|---|
| `--ecp-version` | `0.0.3-alpha` | must be ≥ 0.0.3-alpha; see [below](#why-002-alpha-is-not-enough) |
| `--operator-version` | `1.0.1` | `arubacloud-resource-operator` chart |
| `--regional-url` | `http://127.0.0.1:3001` | what the region catalog advertises; see [below](#why---regional-url-is-a-localhost-address) |
| `--pull-secret` | `ghcr-ecp` | name of the Secret written in both namespaces |
| `--ghcr-user` / `--ghcr-token` | from `gh` | private-registry credential |
| `--jwt-subject` | *(derived)* | grant this subject instead of minting a token |
| `--timeout` | `10m` | per Helm release |
| `--manifests-dir` | *(embedded)* | read `catalog.yaml`/`tenant.yaml` from disk instead |
| `--gateway-image-global` / `--gateway-image-regional` / `--gateway-image-tag` | *(unset)* | point the gateways at a locally built image; the two repository overrides are ignored unless the tag is set |

### `tenant add`

The `seed-tenant.sh` replica: applies `tenant.yaml` and **nothing else**. No
Helm, no gateways, no operator, no delegator, and not the cluster-scoped Region
CR — re-pointing that would change the regional API URL for every tenant at
once. Every object it writes lands in `sha3-224("<tenant>")`, so it cannot
disturb a tenant that is already working.

```shell
./aruba-demo tenant add --tenant ARU-123456 --client-id … --client-secret …
```

With the operator in `auth.mode=single` there is no per-tenant Vault credential
to seed, so this is purely SECA-side: the credentials are used only to derive
the subject to grant, and `--jwt-subject` replaces them entirely. **Every tenant
on the cluster is billed to the one Aruba account the operator holds credentials
for** — a genuinely multi-tenant stack needs `auth.mode=multi`, which is what
[`../install.sh`](../install.sh) does.

`--location` has to match the running stack: the SKU labels record the region as
the gateway stamps it, and a mismatch makes the catalog invisible to its own
tenant. Check it with `aruba-demo versions`, or:

```shell
kubectl -n ecp-system get deploy ecp-gateway-regional \
  -o jsonpath='{.spec.template.spec.containers[0].args}'
```

### `versions` (alias `status`)

What is actually deployed: the Helm release metadata for both namespaces, plus
the container image of every Deployment — the honest answer when a chart was
installed with an image override — and the tenant namespace hash.

```
ecp-system
  release ecp                    chart ecp-0.0.3-alpha  app v0.0.3-alpha  rev 2  deployed
  deploy  ecp-gateway-global     ghcr.io/eu-sovereign-cloud/ecp/gateway-global:v0.0.3-alpha
  deploy  ecp-gateway-regional   ghcr.io/eu-sovereign-cloud/ecp/gateway-regional:v0.0.3-alpha

aruba-system
  release arubacloud-resource-operator   chart arubacloud-resource-operator-1.0.1  app 1.0.0  rev 2  deployed
  release ecp-delegator                  chart ecp-delegator-0.0.3-alpha  app v0.0.3-alpha  rev 2  deployed
  deploy  arubacloud-resource-operator-controller ghcr.io/arubacloud/arubacloud-resource-operator:latest
  deploy  ecp-delegator                  ghcr.io/eu-sovereign-cloud/ecp/delegator-aruba:v0.0.3-alpha

tenant ARU-348095 -> namespace f6517c77a825871ce5e9fdf53dbc516bd510b0a94b5e806e10e008b6
```

It needs no Aruba credential — only the kubeconfig.

### `uninstall`

Removes the three Helm releases. It deliberately leaves the CRDs, the SECA CRs
and the namespaces alone, for the same reason `cluster delete` warns about:
those CRs' finalizers are what reap the real resources in the CMP. Asks for
confirmation unless `--yes` is passed.

## Is the tenant needed?

**Not by the operator, in this single-tenant deployment. Still by SECA.**

`../install.sh` runs the operator in `auth.mode=multi`, where it resolves a
credential *per tenant* out of Vault at `<kvMount>/<tenant>`, keyed by the
`spec.tenant` the aruba plugin stamps on every `arubacloud.com` CR. There the
tenant string has to be the real Aruba account or nothing reconciles.

This CLI runs it in `auth.mode=single`, where the operator holds one
cluster-wide API client in its config Secret and never looks a tenant up. So the
Aruba side no longer cares what the tenant is called.

The SECA side still does, and `--tenant` is still required, because the string
is three other things:

- the tenant segment of every SECA API path
  (`/providers/seca.storage/v1/tenants/<tenant>/skus`),
- the namespace every SECA object for that tenant lands in — the gateway
  computes it as `hex(sha3-224("<tenant>"))`, so it is not a label but an
  address,
- the scope of the `RoleAssignment` that authorizes the caller.

It cannot be derived from the credentials either: the Aruba SDK exposes no
account endpoint, and a `client_credentials` token's `sub` is a service account
user id, not an account number. So it stays a flag, defaulting to `ARU-348095`
as the scripts do. Keeping it equal to the real Aruba account is still the right
habit — it is what `terraform apply -var tenant=…` will use, and what makes the
billing traceable — but nothing enforces it any more.

## Why there is no Vault

`auth.mode=single` puts the API client straight into the operator's config
Secret. That removes the entire dev-mode Vault, and with it three problems the
multi-tenant script has to work around:

- **No `--wait` deadlock.** In `multi`/`auto` the operator Deployment carries an
  initContainer that blocks until `role-id` is populated, and the only thing
  that populates it is a post-install hook Helm does not run until the release's
  resources are ready. Each waits for the other. In `single` there is no
  initContainer, so `install` can wait on every release honestly.
- **No field-manager conflict on the second run.** That same hook writes the
  credentials with `kubectl patch`, so `kubectl-patch` ends up owning fields the
  template still renders as `""`, and every later upgrade needs
  `--force-conflicts`. Nothing patches anything here.
- **Nothing is lost when a pod restarts.** The chart's Vault is dev mode:
  in-memory, no PVC. A restarted `vault-0` comes back with no AppRole, no KV
  mount and no tenant credential, and the stack quietly stops reconciling until
  the install is re-run. A Secret survives.

One quirk to know about: the chart's `vault-auto` templates are gated on
`config.auth.multi.setup` **alone**, not on `config.auth.mode`. At the default
`setup=auto` the `vault-config` Job renders even in single mode and then hangs
forever waiting for a Vault that `vault.enabled=false` never installed. This CLI
therefore sets `config.auth.multi.setup=manual` alongside `mode=single`, which
is what actually drops the hooks.

## Things inherited from the shell stack

### The region has two names

`--region` is the SECA region: the `Region` CR's name, so it has to be a legal
Kubernetes object name — lowercase, `itbg-bergamo`. It is what a client asks for
and what the region catalog is keyed by.

`--location` is what the regional gateway **stamps** on every resource it writes
(`--region` on the binary, frozen at startup), and every aruba converter
forwards that string to the CMP verbatim. Aruba spells it `ITBG-Bergamo` and
does not accept another case:

```
Validation: Name: Location Value: itbg-bergamo not found.
```

The SKU catalogs are labelled with `--location` too, because that label records
the region as the gateway stamps it.

### Why `--regional-url` is a localhost address

A SECA client is given only the **global** endpoint. It resolves the regional API
by reading the provider URLs off the Region CR — so those URLs have to be
reachable **from the client**, not from inside the cluster. With `ClusterIP`
services the client reaches the regional gateway through a port-forward, so that
is what the catalog has to advertise. Expose the gateways through an Ingress or a
LoadBalancer and you set `--regional-url` to the external address and re-run
`install`.

### Why `0.0.2-alpha` is not enough

`ecp-gateway` v0.0.2-alpha decoded the JWT `scope` claim straight into the SECA
down-scope object. OAuth 2.0 defines `scope` as a space-delimited **string**, and
Aruba's Keycloak stamps `"scope":"email"` on a bare `client_credentials` grant —
so the whole token failed to decode and every request came back `401`. Fixed in
v0.0.3-alpha. `--ecp-version` defaults to it.

### Aruba tags must be at least four characters

The API validates every tag against `^[A-Za-z0-9-]{4,30}$` — alphanumerics and
hyphens only, **length 4 to 30** — and the SDK does not check client-side. A tag
one character too short is accepted by the builder, sent, and rejected as:

```
error: create project ecp-demo: HTTP 400: One or more validation errors occurred.
```

with no field named, because the SDK's `HTTPError.Error()` prints only the RFC
7807 title and drops the body's `errors` array. Every Aruba call in this CLI is
wrapped so the field-level errors are appended, and `TestDemoTagsAreValid` pins
the tags this tool stamps (`ecp-demo`, `seca`) against that regex.

### What a missing or wrong credential looks like

The operator builds its Aruba client lazily, on the first reconcile, and a
failure there is a plain error: controller-runtime requeues with backoff and the
resource's phase is **not** touched. So a bad `client_id`/`client_secret` marks
nothing `Failed` — resources simply stay `Pending` and the log fills with
`authentication failed: oauth2: "invalid_client"`. `install` mints a token up
front so this is caught before anything is deployed.

## Layout

| File | What it holds |
|---|---|
| `main.go` | the cobra commands, the flags, credential resolution |
| `aruba.go` | Project / VPC / Subnet / KaaS provisioning and teardown |
| `stack.go` | the install / uninstall / versions flow and the catalog templating |
| `helm.go` | the Helm v4 Go library: upgrade-or-install, uninstall, list, CRD extraction |
| `kube.go` | server-side apply, namespaces, pull secrets, rollout restart and wait |
| `oidc.go` | JWKS → PEM public key, token minting, `sub` extraction |
| `manifests/` | embedded copies of `../catalog.yaml` and `../tenant.yaml` |

Charts are resolved straight from their repository URL, so there is no
`helm repo add` step and nothing is written to your Helm repository config.

`manifests/` are copies, kept so the binary is self-contained. `--manifests-dir ..`
reads the originals from disk instead, and `TestEmbeddedManifestsMatchOriginals`
fails if the two drift.

## Tests

```shell
go test ./...
```

They cover the parts that are wrong silently: the tenant namespace hash against
the value `openssl dgst -sha3-224` produces, that no `@PLACEHOLDER@` survives
into a decoded object, that the JWKS `x5c` certificate becomes a PEM *public
key*, `sub` extraction, `.env` parsing, and that the cluster flag defaults are
real Aruba SDK enum values — a typo there would otherwise fail at KaaS create
time, after the VPC and subnet have already been billed.

The install path was verified end-to-end against a throwaway cluster: all three
charts, the CRD apply, the catalog and tenant apply, a second run for
idempotency, `versions`, `tenant add` and `uninstall`. The `cluster` commands
were not run against the live Aruba API — they create billable resources.
