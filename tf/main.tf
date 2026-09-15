# Create the smallest Aruba VM there is, through the ECP SECA API.
#
#   terraform init && terraform apply
#
# Both port-forwards must be up first (the provider dials the global endpoint,
# then resolves the regional one from the region catalog — see README.md):
#   kubectl -n ecp-system port-forward svc/ecp-gateway-global   3000:80 &
#   kubectl -n ecp-system port-forward svc/ecp-gateway-regional 3001:80 &
#
# A SECA Instance names almost none of what an Aruba CloudServer needs — the VPC, the
# subnets, the security groups and the key pair are all resolved from its dependency
# graph by the aruba plugin. That graph is what most of this file is: everything below
# the instance exists because the instance cannot be created without it.

terraform {
  required_version = ">= 1.13"

  required_providers {
    http = {
      source  = "hashicorp/http"
      version = "~> 3.4"
    }
    seca = {
      source  = "eu-sovereign-cloud/seca"
      version = "~> 0.3"
    }
  }
}

# --------------------------------------------------------------------------------
# Token
# --------------------------------------------------------------------------------
# The gateways verify this JWT against the realm's public RS256 key and authorize
# it against the RoleAssignment that names its `sub`. Nothing else is trusted: the
# token is never introspected, so it stays valid for its full hour
# (`expires_in: 3600`) whatever happens to the client behind it.
data "http" "keycloak_token" {
  url    = "${var.keycloak_url}/realms/${var.realm}/protocol/openid-connect/token"
  method = "POST"

  request_headers = {
    Content-Type = "application/x-www-form-urlencoded"
  }

  request_body = join("&", [
    "grant_type=client_credentials",
    "client_id=${urlencode(var.client_id)}",
    "client_secret=${urlencode(var.client_secret)}",
  ])

  # Without this a bad client id/secret surfaces as "response_body does not have
  # an attribute named access_token" from the jsondecode below, which points at
  # the wrong line entirely.
  lifecycle {
    postcondition {
      condition     = self.status_code == 200
      error_message = "Aruba token endpoint returned ${self.status_code}: ${self.response_body}"
    }
  }
}

locals {
  access_token = jsondecode(data.http.keycloak_token.response_body).access_token
}

# --------------------------------------------------------------------------------
# Provider
# --------------------------------------------------------------------------------
# Only the GLOBAL endpoints are configured. The regional API (storage, workspace,
# network, compute) is discovered from the Region CR the global gateway serves, so
# the URL advertised there is what actually gets dialled.
provider "seca" {
  token  = local.access_token
  tenant = var.tenant
  region = var.region

  # Tuned around a bug in the SDK's polling loop, not just around Aruba being slow.
  # secapi's observer builds an ExponentialBackOff with Multiplier = 0.5 — a
  # multiplier BELOW 1, so each wait is HALF the previous one and the whole series
  # converges to about 2x `interval` no matter how large `max_attempts` is. With the
  # documented-looking 30/10/30 the client gives up ~50s in, which is less than the
  # ~65s a 20 GB volume actually takes.
  #
  # So the knobs that matter are `delay` (when the FIRST check happens) and
  # `interval` (which sets the whole remaining budget at ~2x). A large interval costs
  # nothing when the resource is fast, because the first check still lands at `delay`.
  # `max_attempts` is ignored: the provider overrides it with timeouts/interval.
  retry = {
    delay        = 45
    interval     = 180
    max_attempts = 30
  }

  global_providers = {
    region_v1        = "${var.global_endpoint}/providers/seca.region"
    authorization_v1 = "${var.global_endpoint}/providers/seca.authorization"
  }
}

# --------------------------------------------------------------------------------
# Catalog — read-only, seeded by deploy/aruba-demo/catalog.yaml
# --------------------------------------------------------------------------------
# compute-small is 1 vCPU / 2 GB, which the plugin matches EXACTLY against the Aruba
# flavor table: it is CSO1A2, the smallest flavor Aruba sells. RAM and vCPU both have
# to hit a row of that table or the instance fails with "no Aruba CloudServer flavor
# provides N vCPU / M GB RAM".
data "seca_instance_sku" "small" {
  name = var.instance_sku
}

data "seca_storage_sku" "demo" {
  name = var.storage_sku
}


# --------------------------------------------------------------------------------
# Workspace — the Aruba Project everything else is created in
# --------------------------------------------------------------------------------
resource "seca_workspace" "demo" {
  name = var.workspace_name

  labels = {
    managed-by = "terraform"
  }

  timeouts {
    create = "30m"
    delete = "30m"
  }
}

# --------------------------------------------------------------------------------
# Image, and the volume it is built from
# --------------------------------------------------------------------------------
# Aruba has no image object, and the plugin's image handler is a pure no-op — only the
# image's NAME matters, because that is what maps to an Aruba OS template code
# (ubuntu-24-04 -> LU24-001) on the boot volume below.
#
# It still cannot be a bare catalog entry. A SECA Image is gated on its blockStorageRef
# volume existing and being ACTIVE (ImagePluginHandler.ensureBlockStorageReady), and a
# boot volume is in turn gated on its source image being active — so an Image pointing
# at a volume that does not exist sits in `pending` forever and takes every boot volume
# built from it down with it. Hence a real source volume.
resource "seca_block_storage" "image_src" {
  name         = var.image_source_volume_name
  workspace_id = seca_workspace.demo.name

  size_gb = var.image_source_volume_size_gb
  sku_id  = data.seca_storage_sku.demo.id

  labels = {
    managed-by = "terraform"
  }

  timeouts {
    create = "30m"
    delete = "30m"
  }
}

# The name has to be one skumap knows: alma-8, alma-9, debian-12, ubuntu-22-04,
# ubuntu-24-04, windows-2019, windows-2022. Anything else sends the boot volume to
# `error` with "no Aruba OS template for SECA image".
resource "seca_image" "boot" {
  name             = var.image
  block_storage_id = seca_block_storage.image_src.id

  cpu_architecture = "amd64"
  boot             = "UEFI"
  initializer      = "none"

  labels = {
    managed-by = "terraform"
  }

  timeouts {
    create = "30m"
    delete = "30m"
  }
}

# --------------------------------------------------------------------------------
# Boot volume
# --------------------------------------------------------------------------------
# source_image_id is what makes this a boot disk: the plugin marks the Aruba
# BlockStorage bootable and puts the OS template code on it. Without it the CloudServer
# is refused by the CMP for having no bootable OS.
#
# This volume also fixes the instance's ZONE. Every block storage is created in the
# plugin's hardcoded ITBG-1 datacenter and Aruba requires a server to share its boot
# volume's zone, so var.zone below must stay ITBG-1.
resource "seca_block_storage" "boot" {
  name         = var.boot_volume_name
  workspace_id = seca_workspace.demo.name

  size_gb         = var.boot_volume_size_gb
  sku_id          = data.seca_storage_sku.demo.id
  source_image_id = seca_image.boot.id

  labels = {
    managed-by = "terraform"
  }

  timeouts {
    create = "30m"
    delete = "30m"
  }
}

# --------------------------------------------------------------------------------
# Network
# --------------------------------------------------------------------------------
# The internet gateway creates NOTHING on Aruba — a VPC always has internet egress, so
# there is no gateway object to make. It exists to gate the network: the plugin refuses
# to create the VPC until the SECA resource representing that egress exists.
resource "seca_internet_gateway" "demo" {
  name         = var.internet_gateway_name
  workspace_id = seca_workspace.demo.name

  labels = {
    managed-by = "terraform"
  }
}

resource "seca_network" "demo" {
  name         = var.network_name
  workspace_id = seca_workspace.demo.name

  sku_id = data.seca_network_sku.demo.id

  # cidr is left EMPTY on purpose, and it costs nothing: an Aruba VPC has no address
  # range at all — addressing is per subnet — so Network.Spec.CIDR is one of the SECA
  # fields the plugin explicitly does not propagate. The subnet below carries the range
  # that actually reaches Aruba.
  #
  # Setting it would break every apply. The gateway never populates NetworkStatus.Cidr,
  # so it serialises as "cidr": {} — and the provider reads the network's cidr from
  # status, not spec. Terraform then fails with
  #   .cidr.ipv4: was cty.StringVal("10.30.0.0/16"), but now null
  # and, worse, the null in state makes the next plan want to REPLACE the network, so
  # the run never converges. Unset, the attribute is optional+computed and whatever the
  # provider reports is accepted.
  cidr = {}

  labels = {
    managed-by = "terraform"
  }

  # The plugin gates the VPC on an internet gateway existing in the workspace, and
  # nothing in the SECA schema expresses that link — so it has to be ordered here.
  depends_on = [seca_internet_gateway.demo]

  timeouts {
    create = "30m"
    delete = "30m"
  }
}

data "seca_network_sku" "demo" {
  name = var.network_sku
}

# Aruba has no route table either — routing is a property of the VPC. This is accepted
# and goes active without creating anything, and `routes` never reaches Aruba. It is
# here only because a SECA subnet requires a routeTableRef.
resource "seca_route_table" "demo" {
  name         = var.route_table_name
  workspace_id = seca_workspace.demo.name

  # .name, NOT .id. workspace_id and network_id are PATH parameters, and the gateway
  # hashes the resource's namespace straight from them — sha3-224("<tenant>/<workspace>/
  # <network>") for anything network-scoped. Handing them a full ref hashes the ref
  # string instead, and the write fails against a namespace that does not exist:
  #   namespaces "dcad9f03…" not found
  # The other *_id attributes here are spec REFERENCES and do take the full ref: the
  # plugin parses them (ParseReference / referenceNetwork), so route_table_id,
  # subnet_id, sku_id, source_image_id and boot_volume.device_id all stay .id.
  network_id = seca_network.demo.name

  routes = [{
    destination_cidr_block = var.network_cidr
    target_id              = seca_internet_gateway.demo.id
  }]

  labels = {
    managed-by = "terraform"
  }
}

resource "seca_subnet" "demo" {
  name         = var.subnet_name
  workspace_id = seca_workspace.demo.name
  network_id   = seca_network.demo.name # a path parameter — see the route table above

  route_table_id = seca_route_table.demo.id
  cidr = {
    ipv4 = var.subnet_cidr
  }
  # Not propagated — the Aruba Subnet CRD has no zone field — but required by SECA.
  zone = var.zone

  labels = {
    managed-by = "terraform"
  }

  timeouts {
    create = "30m"
    delete = "30m"
  }
}

# --------------------------------------------------------------------------------
# Security group
# --------------------------------------------------------------------------------
# Creates nothing on its own: a SECA security group is workspace-scoped and carries no
# VPC, while an Aruba SecurityGroup requires one. The real Aruba SecurityGroup and its
# SecurityRules are materialised by the INSTANCE handler, per VPC, when a NIC attaches
# this group — named "<group>-<network>". Deleting this resource is what reaps them.
#
# An Aruba CloudServer requires at least one security group, so this is not optional.
resource "seca_security_group" "demo" {
  name         = var.security_group_name
  workspace_id = seca_workspace.demo.name

  # rule_refs, source_refs and ports.list are set to [] deliberately. Left unset, the
  # provider reads them back as empty lists and Terraform fails the apply with
  # "Provider produced inconsistent result after apply: .rules[0].source_refs: was
  # null, but now cty.ListValEmpty(cty.String)". Writing the empty lists in the config
  # makes plan and result agree. No source means all traffic, i.e. 0.0.0.0/0.
  rule_refs = []

  rules = [{
    direction   = "ingress"
    protocol    = "tcp"
    source_refs = []
    ports = {
      from = 22
      to   = 22
      list = []
    }
  }]

  labels = {
    managed-by = "terraform"
  }
}

# --------------------------------------------------------------------------------
# Public IP
# --------------------------------------------------------------------------------
# An Aruba ElasticIP. Attached to the NIC below, which is where the instance handler
# looks for it: the CloudServer takes the FIRST public IP of its first NIC, so a
# second one here would be created and then ignored.
#
# `address` is deliberately not set — Aruba always allocates the address itself and
# the plugin rejects a SECA PublicIp that asks for a specific one (BYOIP). `version`
# has to be IPv4: an ElasticIP is IPv4-only and IPv6 is rejected outright.
#
# It also gates the server: like the boot volume and the key pair, the ElasticIP has
# to be Active in the CMP before the CloudServer is created, so this adds a step to
# the create rather than happening alongside it.
#
# ADDING THIS TO A LIVE INSTANCE DOES NOTHING BY ITSELF. The handler reads the first
# NIC's public IP only while building the CloudServer; the instance's Update path
# re-applies tags and the key pair and nothing else, so Terraform reporting the NIC
# as "updated in-place" is real on the SECA side and inert on the Aruba one — the
# address is allocated but never attached. An existing server has to be recreated to
# pick it up:
#     terraform apply -replace=seca_instance.demo
resource "seca_public_ip" "demo" {
  name         = var.public_ip_name
  workspace_id = seca_workspace.demo.name

  version = "IPv4"

  labels = {
    managed-by = "terraform"
  }

  timeouts {
    create = "30m"
    delete = "30m"
  }
}

# --------------------------------------------------------------------------------
# NIC
# --------------------------------------------------------------------------------
# Aruba has no NIC object — a CloudServer's networking is inline. This is consumed, not
# created: the plugin follows it to the subnet (which fixes the VPC) and to the security
# groups it has to materialise there.
resource "seca_nic" "demo" {
  name         = var.nic_name
  workspace_id = seca_workspace.demo.name

  subnet_id          = seca_subnet.demo.id
  security_group_ids = [seca_security_group.demo.id]
  addresses          = [var.nic_address]
  public_ip_ids      = [seca_public_ip.demo.id]

  labels = {
    managed-by = "terraform"
  }
}

# --------------------------------------------------------------------------------
# Instance
# --------------------------------------------------------------------------------
# ssh_keys is documented as "references" but carries the key MATERIAL: the plugin builds
# an Aruba KeyPair named "<instance>-key" from the first entry, because a CloudServer
# requires a KeyPairReference and SECA has no key-pair resource. Aruba validates the
# format, so it has to be a well-formed public key.
#
# `zone` has to be set — the CRD refuses an empty one
# ("spec.zone in body should be at least 1 chars long") — and it has to be exactly the
# boot volume's zone. The plugin compares the two as plain strings and refuses an
# instance that disagrees, and every block storage is pinned to the plugin's hardcoded
# ITBG-1, so ITBG-1 is the only value that can work today.
resource "seca_instance" "demo" {
  name         = var.instance_name
  workspace_id = seca_workspace.demo.name

  sku_id         = data.seca_instance_sku.small.id
  primary_nic_id = seca_nic.demo.id
  ssh_keys       = [var.ssh_public_key]
  zone           = var.zone

  boot_volume = {
    device_id = seca_block_storage.boot.id
  }

  labels = {
    managed-by = "terraform"
  }

  # Every Aruba resource the server references — boot volume, key pair, materialised
  # security groups — must already be Active in the CMP: Aruba rejects a server whose
  # references are not provisioned yet, and the rejection is TERMINAL, not retried.
  timeouts {
    create = "45m"
    delete = "30m"
  }
}

output "workspace" {
  value = seca_workspace.demo.name
}

output "instance_id" {
  value = seca_instance.demo.id
}

output "instance_flavor" {
  description = "The Aruba flavor the SKU resolves to."
  value       = "${data.seca_instance_sku.small.vcpus} vCPU / ${data.seca_instance_sku.small.ram} GB"
}

output "instance_power_state" {
  value = seca_instance.demo.power_state
}

# ALWAYS EMPTY on Aruba, and it cannot be otherwise: the operator's ElasticIP CRD has
# no address field at all (status carries only phase/projectID/resourceID), so the
# address Aruba allocates has nowhere to travel back through. Kept because it is the
# correct SECA-level expression of "the address", and a CSP that reports one would
# fill it in. Get the real address from the CMP — see README, "Finding the address".
output "instance_public_ip" {
  description = "Empty on Aruba — the ElasticIP CRD carries no address. See README."
  value       = seca_public_ip.demo.ip_address
}

# The IDs needed to look the address up in the Aruba CMP.
output "public_ip_name" {
  value = seca_public_ip.demo.name
}
