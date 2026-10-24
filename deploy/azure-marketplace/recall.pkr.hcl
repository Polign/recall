# Builds the Recall image listed on Azure Marketplace.
#
# The image carries the Recall and polign_db release binaries, the built-in
# embedding model, systemd units for both, and a first-boot script that
# resolves the store and generates the instance's own keys. Recall listens on
# :8080; the database behind it listens on localhost only. The image carries
# no credentials and no store configuration: a launch supplies the storage
# account and container through a VM tag or /etc/recall/recall.env, and the
# VM's managed identity is how the database reaches it.
#
#   az login
#   packer init .
#   packer build -var recall_version=v0.23.0 .
#
# The build publishes straight into the Azure Compute Gallery that Partner
# Center reads from. The gallery, its image definition, and the resource group
# have to exist first (see README.md); Packer only adds a version. The
# subscription matters the way the seller account matters on AWS: Partner
# Center can only pick images from a subscription in the tenant the Partner
# Center account belongs to, so the build checks the active subscription and
# stops with the mismatch named.

packer {
  required_plugins {
    azure = {
      source  = "github.com/hashicorp/azure"
      version = "~> 2.0"
    }
  }
}

variable "recall_version" {
  type        = string
  default     = "v0.23.0"
  description = "Recall release tag to install (must be a published, non-draft release)."
}

variable "polign_version" {
  type        = string
  default     = "v0.15.0"
  description = "polign_db release tag to install as the backend."
}

variable "subscription_id" {
  type        = string
  default     = "a19d6e8f-f315-4df4-be3e-6c4659ce0ba8"
  description = <<-EOT
    The subscription that holds the gallery. It has to be in the same Entra
    tenant as the Partner Center account (polign.onmicrosoft.com), because
    the offer's technical configuration lists gallery images from that tenant
    only. The default is the Polign subscription created for this purpose.
  EOT
}

variable "resource_group" {
  type        = string
  default     = "polign-marketplace"
  description = "Resource group holding the gallery. The build VM is created here too, so nothing needs permission to create resource groups."
}

variable "gallery_name" {
  type        = string
  default     = "polign_marketplace"
  description = "Azure Compute Gallery the version is published into."
}

variable "arch" {
  type        = string
  default     = "amd64"
  description = <<-EOT
    Target architecture: amd64 (x64) or arm64 (Cobalt / Ampere sizes).

    amd64 first, as on Google Cloud: Arm sizes are offered in fewer regions,
    and a Marketplace listing is expected to launch wherever it is offered.
    An arm64 build publishes into the recall-arm64 image definition, which
    has to be created the same way as recall-x64 (README.md).
  EOT

  validation {
    condition     = contains(["arm64", "amd64"], var.arch)
    error_message = "The arch variable must be arm64 or amd64."
  }
}

variable "vm_size" {
  type        = string
  default     = ""
  description = "Build VM size. Empty picks a small Generation 2 size for the architecture."
}

variable "disk_size" {
  type        = number
  default     = 30
  description = "OS disk size in GB. Holds the local disk cache tier, so leave room above the cache budget."
}

variable "replication_regions" {
  type        = list(string)
  default     = ["westus2"]
  description = <<-EOT
    Regions the gallery version is replicated to. Partner Center copies the
    image to every Marketplace region itself, so the build only needs the
    gallery's home region. Add regions here if you want to launch test
    instances elsewhere before submitting.
  EOT
}

locals {
  # Debian publishes Generation 2 SKUs for both architectures under one offer.
  image_sku = var.arch == "arm64" ? "12-arm64" : "12-gen2"
  # Sizes the Polign subscription can launch without a quota request: the v5
  # families came with zero quota and the x64 v6 sizes were restricted in
  # westus2, while Dsv7 (x64) and Dpsv6 (Arm, zone 2 only) were open with 10
  # cores each. Any Gen2 size works; override with -var vm_size if a region
  # lacks these.
  vm_size   = var.vm_size != "" ? var.vm_size : (var.arch == "arm64" ? "Standard_D2ps_v6" : "Standard_D2s_v7")
  image_def = var.arch == "arm64" ? "recall-arm64" : "recall-x64"
  timestamp = formatdate("YYYYMMDDhhmmss", timestamp())

  # Gallery versions are semantic: MAJOR.MINOR.PATCH, digits only, so v0.7.0
  # becomes 0.7.0. A version already in the gallery cannot be overwritten.
  # Rebuilding the same release (a base-image patch, a script fix) therefore
  # needs -var gallery_version with a bumped PATCH, for example 0.7.1, and the
  # Partner Center plan is then pointed at the new version.
  gallery_version = var.gallery_version != "" ? var.gallery_version : trimprefix(var.recall_version, "v")
}

variable "gallery_version" {
  type        = string
  default     = ""
  description = "Gallery image version to publish. Empty derives it from recall_version (v0.23.0 -> 0.22.1). A version already in the gallery cannot be overwritten."
}

source "azure-arm" "recall" {
  use_azure_cli_auth = true
  subscription_id    = var.subscription_id

  # Building inside the gallery's resource group means the build needs no
  # right to create resource groups, and the location is the group's.
  build_resource_group_name = var.resource_group

  os_type         = "Linux"
  image_publisher = "Debian"
  image_offer     = "debian-12"
  image_sku       = local.image_sku
  vm_size         = local.vm_size
  os_disk_size_gb = var.disk_size

  ssh_username = "packer"

  # Publish directly into the gallery. No intermediate managed image is kept,
  # so the only artifact left behind is the version Partner Center consumes.
  shared_image_gallery_destination {
    subscription         = var.subscription_id
    resource_group       = var.resource_group
    gallery_name         = var.gallery_name
    image_name           = local.image_def
    image_version        = local.gallery_version
    replication_regions  = var.replication_regions
    storage_account_type = "Standard_LRS"
  }

  azure_tags = {
    product        = "recall"
    recall-version = var.recall_version
    polign-version = var.polign_version
    architecture   = var.arch
    built-at       = local.timestamp
  }
}

build {
  name    = "recall"
  sources = ["source.azure-arm.recall"]

  # Fail before provisioning if the active Azure CLI login is pointed at some
  # other subscription. Runs on the build host, costs nothing, names the
  # mismatch.
  provisioner "shell-local" {
    inline = [
      "set -eu",
      "got=$(az account show --query id -o tsv 2>/dev/null || true)",
      "if [ -z \"$got\" ]; then echo 'not logged in: run az login' >&2; exit 1; fi",
      "if [ \"$got\" != '${var.subscription_id}' ]; then",
      "  echo \"active subscription $got is not the gallery subscription ${var.subscription_id}\" >&2",
      "  echo \"run: az account set --subscription ${var.subscription_id}\" >&2",
      "  exit 1",
      "fi",
      "echo \"building into ${var.subscription_id} / ${var.resource_group} / ${var.gallery_name} / ${local.image_def} ${local.gallery_version}\"",
    ]
  }

  # The file provisioner will not create the destination directory itself.
  provisioner "shell" {
    inline = ["mkdir -p /tmp/recall-files"]
  }

  provisioner "file" {
    source      = "files/"
    destination = "/tmp/recall-files"
  }

  provisioner "shell" {
    environment_vars = [
      "RECALL_VERSION=${var.recall_version}",
      "POLIGN_VERSION=${var.polign_version}",
      "ARCH=${var.arch}",
    ]
    scripts = [
      "scripts/install.sh",
      "scripts/harden.sh",
      "scripts/cleanup.sh",
    ]
    execute_command = "{{ .Vars }} sudo -E bash -eux '{{ .Path }}'"
  }

  # Generalize. This is what Azure certification checks for: the agent wipes
  # host keys, DHCP leases, the hostname, and (+user) the packer account, so
  # the version boots as a fresh machine. It has to be the last step.
  provisioner "shell" {
    execute_command = "chmod +x {{ .Path }}; {{ .Vars }} sudo -E sh '{{ .Path }}'"
    inline = [
      "/usr/sbin/waagent -force -deprovision+user && export HISTSIZE=0 && sync",
    ]
    inline_shebang = "/bin/sh -x"
  }

  post-processor "manifest" {
    output     = "manifest.json"
    strip_path = true
    custom_data = {
      recall_version  = var.recall_version
      polign_version  = var.polign_version
      arch            = var.arch
      subscription_id = var.subscription_id
      gallery_version = local.gallery_version
    }
  }
}
