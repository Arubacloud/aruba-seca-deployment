# Terraform: the smallest Aruba VM, through ECP

Creates a SECA compute instance — 1 vCPU / 2 GB, the smallest flavor Aruba sells —
together with the whole dependency graph an Aruba `CloudServer` needs. Everything
here is a real, billable resource in the account named by `tenant`.

The stack this drives is [`../deploy/aruba-demo`](../deploy/aruba-demo); install
that first.

## Run it

> **The released provider cannot apply this config.** 0.3.1 (2026-08-04) fails on the
> NIC with `Provider produced inconsistent result after apply: .public_ip_ids: element
> 0 has vanished` — the write lands, the read-back is empty, the NIC is left tainted
> and every retry loops. Fixed by #99 on `main`, along with #96 (URN scope refs) and
> #98 (network cidr merge), none of them released yet. Build `main` and point
> Terraform at it:
>
> ```shell
> git clone https://github.com/eu-sovereign-cloud/terraform-provider-seca.git
> cd terraform-provider-seca && go build -o ~/.terraform.d/dev/eu-sovereign-cloud/seca/terraform-provider-seca .
>
> cat > ~/.terraformrc <<'EOF'
> provider_installation {
>   dev_overrides {
>     "eu-sovereign-cloud/seca" = "/home/<you>/.terraform.d/dev/eu-sovereign-cloud/seca"
>   }
>   direct {}
> }
> EOF
> ```
>
> Delete `~/.terraformrc` once a release past 0.3.1 exists — a dev override applies to
> every Terraform project on the machine and makes Terraform ignore the lock file.

```shell
# the provider dials the global endpoint, then resolves the regional one from
# the region catalog — both port-forwards have to be up. Use the kubeconfig of the
# cluster you actually mean: the client-cli writes its own
# (deploy/aruba-demo/client-cli/ecp-demo.kubeconfig) rather than touching ~/.kube/config.
kubectl -n ecp-system port-forward svc/ecp-gateway-global   3000:80 &
kubectl -n ecp-system port-forward svc/ecp-gateway-regional 3001:80 &

cp terraform.tfvars.example terraform.tfvars   # then fill in the API client
terraform init
terraform apply
```

Tear down with `terraform destroy` **before** deleting the cluster, or the CRs'
finalizers never run and the VM and its disks are stranded in the Aruba CMP.

## What it creates, and why

A SECA `Instance` names almost nothing an Aruba `CloudServer` needs. The VPC, the
subnets, the security groups and the key pair are all resolved by the aruba plugin
from the instance's dependency graph — so that graph is most of this config.

```
workspace ─────────────────────────────────────────────►  Project
  ├─ block_storage.image_src ──► image ──► block_storage.boot ──► BlockStorage (bootable, LU24-001)
  ├─ internet_gateway (no-op, gates the network) ──► network ──►  VPC
  │                                    └─ route_table (no-op) ──► subnet ──► Subnet
  ├─ security_group (no-op until attached) ──┐
  ├─ public_ip ──────────────────────────────┤ ──►             ElasticIP
  └─ nic (no-op; consumed) ──────────────────┴──► instance ──►  CloudServer
                                                               + SecurityGroup + SecurityRule
                                                               + KeyPair
```

Four of these create **nothing** on Aruba and exist only so the SECA model is
complete: the internet gateway (an Aruba VPC always has egress — but the plugin
refuses to create the VPC until one exists), the route table (routing is a VPC
property; required because a SECA subnet needs a `routeTableRef`), the NIC (Aruba
has no NIC object) and the security group (it has no VPC, so it cannot become an
Aruba `SecurityGroup` until an instance attaches it to one).

ECP's [`csp/aruba/README.md`](https://github.com/eu-sovereign-cloud/ecp/blob/main/csp/aruba/README.md) is the full mapping and is worth reading before changing
anything here.

## Variables

Everything has a default except the credentials — see
[`variables.tf`](variables.tf). The ones you would actually change:

| Variable | Default | Notes |
|---|---|---|
| `client_id`, `client_secret` | _(required)_ | The Aruba API client. Put them in `terraform.tfvars` (gitignored). |
| `tenant` | `ARU-348095` | The Aruba account. Resources are billed to it. |
| `region` | `itbg-bergamo` | The SECA region — the **Region CR's name**, not the Aruba location. See below. |
| `instance_sku` | `compute-small` | 1 vCPU / 2 GB → Aruba `CSO1A2`. Must match a flavor exactly. |
| `image` | `ubuntu-24-04` | Only the *name* matters; it maps to an Aruba OS template code. |
| `ssh_public_key` | _(a test key)_ | Aruba validates the format. Replace it to actually log in. |
| `global_endpoint` | `http://localhost:3000` | The global gateway's port-forward. |

## Things that took a while to find

Each of these is a wrong-looking failure with a right answer somewhere else.

### The region has two spellings and they are not interchangeable

`region` here is the SECA region: the name of the `Region` CR, so it must be a
legal Kubernetes object name — lowercase `itbg-bergamo`. What the regional gateway
**stamps** on every resource is a separate value (`--region`, set by `install.sh`
to `ITBG-Bergamo`), and every aruba converter forwards that string to the CMP
verbatim as the location. Aruba is not case-insensitive about it:

```
Validation: Name: Location Value: itbg-bergamo not found.
```

The old workspace + block-storage demo never hit this: the Project converter sets
no region at all, and a block storage takes its region from its (region-less) refs
and falls through to the plugin's own `ITBG-Bergamo` default. **Everything else** —
VPC, subnet, elastic IP, security group, key pair, CloudServer — forwards the
stamped value, so the whole network and compute half of the API depends on it.

### `workspace_id` and `network_id` take a name; every other `*_id` takes a ref

They are path parameters, and the gateway hashes a resource's namespace straight
out of them — `sha3-224("<tenant>/<workspace>/<network>")` for anything
network-scoped. Give `network_id` a full ref and the ref *string* gets hashed, and
the write lands in a namespace that does not exist:

```
namespaces "dcad9f03…" not found
```

The other `*_id` attributes are spec references, which the plugin parses
(`ParseReference`, `referenceNetwork`), so `route_table_id`, `subnet_id`, `sku_id`,
`source_image_id` and `boot_volume.device_id` all take the full `.id`.

### The image needs a real volume behind it

A SECA `Image` is gated on its `blockStorageRef` volume existing and being
**active**, and a boot volume is gated on its source image being active. So an
`Image` seeded into the catalog with a nominal reference sits in `pending` forever
and takes every boot volume built from it down with it. Hence
`seca_block_storage.image_src`, which exists only to be pointed at.

Aruba itself has no image object — the OS is a template code on the boot volume —
so the image's *name* is the only load-bearing thing about it.

### The instance's zone must be set, and must be `ITBG-1`

Omitting it fails the CRD (`spec.zone in body should be at least 1 chars long`).
Setting anything else fails the plugin, which compares the instance's zone against
its boot volume's as plain strings — and every block storage is pinned to the
plugin's hardcoded `ITBG-1` datacenter, because SECA models no per-volume zone.

### Three provider bugs are worked around in the config

All three are "Provider produced inconsistent result after apply", where the
provider returns a value the config did not ask for:

| Attribute | Symptom | Workaround here |
|---|---|---|
| `security_group.rule_refs`, `.rules[].source_refs`, `.rules[].ports.list` | `was null, but now cty.ListValEmpty` | write the empty lists explicitly |
| `network.cidr.ipv4` | `was "10.30.0.0/16", but now null` | leave `cidr = {}` |

The network one is the nasty one. The gateway never populates
`NetworkStatus.Cidr`, so it serialises as `"cidr": {}`, and the provider reads the
network's cidr from **status** rather than spec — so it is always null. The null in
state then makes the *next* plan want to replace the network, and the run never
converges. Leaving it empty costs nothing: an Aruba VPC has no address range at
all, and `Network.Spec.CIDR` is one of the fields the plugin explicitly does not
propagate. The subnet's CIDR is the one that reaches Aruba, and it round-trips fine.

### A public IP only attaches at instance-create time

`seca_public_ip` plus `public_ip_ids` on the NIC is what gets you an address you can
SSH to, but adding it to a **running** instance changes nothing: the plugin reads the
first NIC's public IP only while building the CloudServer, and the instance's `Update`
re-applies tags and the key pair and nothing else. Terraform will happily report the
NIC as "updated in-place" — true on the SECA side, inert on the Aruba one. Recreate
the server to pick the address up:

```shell
terraform apply -replace=seca_instance.demo
```

Which is also the moment to set a real `ssh_public_key`: the default is the
integration suite's test key and nobody has its private half, so the address is
useless without it.

### Finding the address, and logging in

`instance_public_ip` is **always empty on Aruba** and cannot be anything else: the
operator's `ElasticIP` CRD has no address field — its status carries only
`phase`/`projectID`/`resourceID` — so the address Aruba allocates has nowhere to
travel back through SECA. Read it from the CMP instead, using the two ids the
`ElasticIP` CR does report:

```shell
EIP=$(kubectl get elasticip.arubacloud.com -A -o jsonpath='{.items[0].status}')
# -> {"projectID":"…","resourceID":"…"}

curl -s -H "Authorization: Bearer $TOKEN" \
  "https://api.arubacloud.com/projects/<projectID>/providers/Aruba.Network/elasticIps/<resourceID>" \
  | jq '{address: .properties.address, state: .status.state}'
```

`state: "InUse"` means it is attached to the server.

**The login user is `ubuntu`, not `root`** — it is the stock Ubuntu cloud image and
that is the account cloud-init puts the key on:

```shell
ssh ubuntu@<address>
```

### The instance reports `power_state = "off"`

Expected. Aruba's `CloudServer` CRD exposes no power field, so the plugin's
`PowerOn`/`PowerOff` are no-ops and SECA's power state never advances. The VM
itself is running — check the `cloudserver.arubacloud.com` CR, which reports
`Active`, or the Aruba console.

### The `retry` block is tuned around an SDK bug, not just latency

`secapi`'s observer builds its `ExponentialBackOff` with `Multiplier = 0.5`, so
each wait is *half* the previous one and the total budget converges to about
`2 x interval` regardless of `max_attempts`. `delay` (when the first check happens)
and `interval` (the whole remaining budget) are the only knobs that do anything;
`max_attempts` is overridden by the provider from `timeouts / interval`.

### The token lasts 1 hour

`expires_in: 3600` from the realm, re-fetched on each plan/apply. A full apply of
this stack takes ~15 minutes, so it never outlives it. Verification is offline, so
revoking the API client does not cut off a token already issued — that window is
the hour. Deleting the RoleAssignment is immediate.
