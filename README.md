# aruba-seca-deployment

Installs [ECP](https://github.com/eu-sovereign-cloud/ecp), the European Control Plane, against Aruba Cloud and onboards tenants onto it, so SECA clients can manage Aruba resources through the SECA API.

## What is in here

| Path | What |
|---|---|
| [`deploy/aruba-demo/client-cli`](deploy/aruba-demo/client-cli) | `aruba-demo`, a Go CLI: creates an Aruba KaaS cluster, installs ECP, seeds tenants |
| [`deploy/aruba-demo`](deploy/aruba-demo) | `install.sh` and `seed-tenant.sh`: the same install and seeding, on a cluster you already have |
| [`tf`](tf) | Terraform for the demo VM, created through the SECA API |

The CLI and the scripts are the same idea, split into the same two parts:

1. **Cluster and ECP**: `aruba-demo up` (creates the cluster, then installs) or `install.sh` (existing cluster). Installs the Aruba resource operator, the ECP gateways and delegator, and the region catalog, and seeds a first tenant.
2. **Tenant seeding**: `aruba-demo tenant add` or `seed-tenant.sh`. Adds one tenant: its namespace, SKU catalogue and permissions (Role + RoleAssignment).

On a multi-tenant installation the cluster and ECP are already there, so onboarding a tenant is only part 2. Note that the scripts run the operator multi-tenant, with one Aruba credential per tenant in Vault, while the CLI holds a single credential that every tenant is billed to.

The Terraform is not part of the deployment: it is a use case of ECP, the demo VM created once the stack is up.

## Prerequisites

- An Aruba API client (`ARUBA_CLIENT_ID` / `ARUBA_CLIENT_SECRET`).
- `gh` logged in with the `read:packages` scope: the ECP images are private.
- CLI: Go. Scripts: `kubectl`, `helm`, `jq`, `openssl`, `curl`. Demo: `terraform >= 1.13`.

> ⚠️ Everything here creates real, billable Aruba resources.

## Usage

```shell
export ARUBA_CLIENT_ID=<client-id> ARUBA_CLIENT_SECRET=<client-secret>
```

With the CLI, on a new KaaS cluster:

```shell
cd deploy/aruba-demo/client-cli && go build -o aruba-demo .
./aruba-demo up --tenant ARU-111111          # 1. cluster + ECP
./aruba-demo tenant add --tenant ARU-222222  # 2. another tenant
```

With the scripts, on an existing cluster:

```shell
cd deploy/aruba-demo
KUBE_CONTEXT=<context> ARUBA_TENANT=ARU-111111 ./install.sh      # 1. ECP
# 2. another tenant, with that tenant's own Aruba credentials
KUBE_CONTEXT=<context> ARUBA_TENANT=ARU-222222 \
ARUBA_CLIENT_ID=… ARUBA_CLIENT_SECRET=… ./seed-tenant.sh
```

The demo VM, with both gateways port-forwarded (it needs the SECA Terraform provider built from `main`; see [`tf/README.md`](tf/README.md)):

```shell
kubectl -n ecp-system port-forward svc/ecp-gateway-global   3000:80 &
kubectl -n ecp-system port-forward svc/ecp-gateway-regional 3001:80 &
cd tf && cp terraform.tfvars.example terraform.tfvars  # fill in the credentials
terraform init && terraform apply -var tenant=ARU-111111
```

Run `terraform destroy` **before** deleting the cluster, or the VM and its disks are left behind in Aruba.

Each folder's README has the details.
