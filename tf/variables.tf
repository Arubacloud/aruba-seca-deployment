variable "keycloak_url" {
  description = "Base URL of the Aruba IdP, without the realm path."
  type        = string
  default     = "https://mylogin.aruba.it/auth"
}

variable "realm" {
  description = "Keycloak realm that mints the API token."
  type        = string
  default     = "cmp-new-apikey"
}

variable "client_id" {
  description = "Aruba API client id. The SAME client the arubacloud-resource-operator is installed with."
  type        = string
  sensitive   = true
}

variable "client_secret" {
  description = "Aruba API client secret."
  type        = string
  sensitive   = true
}

variable "tenant" {
  description = "Aruba account, which is also the SECA tenant. Real resources are billed to it."
  type        = string
  default     = "ARU-348095"
}

variable "region" {
  description = "SECA region. Must name a Region CR the global gateway serves."
  type        = string
  default     = "itbg-bergamo"
}

variable "global_endpoint" {
  description = "Where the global gateway is reachable (kubectl port-forward svc/ecp-gateway-global 3000:80)."
  type        = string
  default     = "http://localhost:3000"
}

# --- catalog (read-only, seeded by deploy/aruba-demo/catalog.yaml) ---------------

variable "instance_sku" {
  description = "InstanceSKU name. Its vCPU/RAM must match an Aruba flavor exactly; compute-small is 1/2 = CSO1A2, the smallest."
  type        = string
  default     = "compute-small"
}

variable "storage_sku" {
  description = "Name of a StorageSKU the region catalog publishes for this tenant."
  type        = string
  default     = "storage-standard"
}

variable "network_sku" {
  description = "NetworkSKU name. Nothing on the Aruba side reads it, but a SECA network requires one."
  type        = string
  default     = "network-standard"
}

variable "image_source_volume_name" {
  description = "Plain volume the image is built from. Only needed because a SECA Image is gated on a real, active block storage."
  type        = string
  default     = "demo-imgsrc"
}

variable "image_source_volume_size_gb" {
  type    = number
  default = 20
}

variable "image" {
  description = "Image name. Only the NAME matters — the plugin maps it to an Aruba OS template code, so it must be one of alma-8, alma-9, debian-12, ubuntu-22-04, ubuntu-24-04, windows-2019, windows-2022."
  type        = string
  default     = "ubuntu-24-04"
}

# --- what to create -------------------------------------------------------------
# Aruba rejects any resource name shorter than 4 characters with a bare 400.

variable "workspace_name" {
  type    = string
  default = "demo-ws"
}

variable "boot_volume_name" {
  type    = string
  default = "demo-boot"
}

variable "boot_volume_size_gb" {
  description = "Boot disk size. 20 GB matches the Aruba CRD default and leaves room for an OS image."
  type        = number
  default     = 20
}

variable "internet_gateway_name" {
  type    = string
  default = "demo-igw"
}

variable "network_name" {
  type    = string
  default = "demo-net"
}

variable "network_cidr" {
  type    = string
  default = "10.30.0.0/16"
}

variable "route_table_name" {
  type    = string
  default = "demo-rtb"
}

variable "subnet_name" {
  type    = string
  default = "demo-subnet"
}

variable "subnet_cidr" {
  type    = string
  default = "10.30.1.0/24"
}

variable "security_group_name" {
  type    = string
  default = "demo-sg"
}

variable "public_ip_name" {
  type    = string
  default = "demo-ip"
}

variable "nic_name" {
  type    = string
  default = "demo-nic"
}

variable "nic_address" {
  type    = string
  default = "10.30.1.10"
}

variable "instance_name" {
  type    = string
  default = "demo-vm"
}

variable "zone" {
  description = "Zone for the subnet and the instance. Must be ITBG-1: the plugin pins every block storage to that datacenter and refuses an instance whose zone differs from its boot volume's."
  type        = string
  default     = "ITBG-1"
}

variable "ssh_public_key" {
  description = "Public key material for the instance. The plugin builds an Aruba KeyPair from it and Aruba validates the format, so it must be a real public key. The default is the integration suite's test key — NOBODY HAS ITS PRIVATE HALF, so override it or the public IP is useless to you."
  type        = string
  default     = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMp3nK7rhAOCKmmcYVLPHZPH5qEjzMk4HcUlq/3yRj7T francesco29.fl@gmail.com"
}
