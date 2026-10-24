# Builds the Recall Compute Engine image listed on Google Cloud Marketplace.
#
# The image carries the Recall and polign_db release binaries, the built-in
# embedding model, systemd units for both, and a first-boot script that
# resolves the store and generates the instance's own keys. Recall listens on
# :8080; the database behind it listens on localhost only. The image carries
# no credentials and no store configuration: the Terraform module that ships
# with the listing supplies the bucket, the service account, and a
# VPC-scoped firewall rule.
#
#   packer init .
#   packer build -var project_id=polign-admin -var recall_version=v0.23.0 .
#
# The project matters the same way the seller account matters on AWS: the
# Producer Portal ingests images from the producer project you registered, so
# an image built anywhere else cannot be submitted. The build checks the
# project before provisioning and stops with the mismatch named.

packer {
  required_plugins {
    googlecompute = {
      source  = "github.com/hashicorp/googlecompute"
      version = "~> 1.1"
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

variable "project_id" {
  type        = string
  description = <<-EOT
    The Google Cloud project the image is built into. This must be the
    producer project registered in Producer Portal: Marketplace ingests from
    that project, so an image built anywhere else has to be rebuilt. There is
    no default on purpose, because a wrong default is how the AWS v0.4.1 build
    ended up in an account it could never be submitted from.
  EOT
}

variable "zone" {
  type        = string
  default     = "us-central1-a"
  description = <<-EOT
    Build zone. The image itself is a global resource, so this only picks
    where the short-lived build instance runs. Keep it somewhere the chosen
    architecture is offered: Arm (T2A and C4A) is available in far fewer zones
    than x86.
  EOT
}

variable "arch" {
  type        = string
  default     = "amd64"
  description = <<-EOT
    Target architecture: amd64 (x86_64) or arm64 (Ampere Altra / Axion).

    This defaults to amd64, the opposite of the AWS build, because Arm on
    Google Cloud is offered in a much smaller set of zones. A Marketplace
    listing is expected to launch in every region it offers, so the broad
    architecture is the safer primary. Build arm64 as a second image once the
    listing's region list is settled.
  EOT

  validation {
    condition     = contains(["arm64", "amd64"], var.arch)
    error_message = "The arch variable must be arm64 or amd64."
  }
}

variable "machine_type" {
  type        = string
  default     = ""
  description = "Build machine type. Empty picks a small default for the architecture."
}

variable "disk_size" {
  type        = number
  default     = 30
  description = "Boot disk size in GB. Holds the local disk cache tier, so leave room above the cache budget."
}

variable "image_licenses" {
  type        = list(string)
  default     = []
  description = <<-EOT
    Marketplace license to attach to the image, as a full self link:
      projects/<producer-project>/global/licenses/<license-id>

    Producer Portal mints this when the listing's pricing is configured, and
    it is what associates a running instance with the listing. Leave empty
    for a build you are only testing: an image with no license still boots,
    it just is not a Marketplace image. Attaching a license is not reversible
    on an existing image, so a licensed build is a fresh build.
  EOT
}

variable "image_storage_locations" {
  type        = list(string)
  default     = []
  description = "Optional storage location for the image, for example [\"us\"]. Empty uses the multi-region nearest the build zone."
}

locals {
  # Debian's Arm images use a separate family; the release tarballs use
  # "amd64"/"arm64" for both.
  source_family = var.arch == "arm64" ? "debian-12-arm64" : "debian-12"
  machine_type  = var.machine_type != "" ? var.machine_type : (var.arch == "arm64" ? "t2a-standard-2" : "e2-medium")
  timestamp     = formatdate("YYYYMMDDhhmmss", timestamp())

  # Image names are DNS-like: lowercase, dashes only, 63 characters at most.
  # A version tag with dots is not a legal name, so v0.5.0 becomes v0-5-0.
  name = "recall-${replace(var.recall_version, ".", "-")}-${var.arch}-${local.timestamp}"
}

source "googlecompute" "recall" {
  project_id   = var.project_id
  zone         = var.zone
  machine_type = local.machine_type

  source_image_family     = local.source_family
  source_image_project_id = ["debian-cloud"]
  image_name              = local.name
  image_family            = "recall-${var.arch}"
  image_description       = "Recall ${var.recall_version} on polign_db ${var.polign_version} (${var.arch}) on Debian 12"
  image_licenses          = var.image_licenses
  image_storage_locations = var.image_storage_locations

  # Guest OS features Marketplace expects on a modern VM image. UEFI_COMPATIBLE
  # is what lets a buyer turn on Shielded VM; GVNIC and the multiqueue SCSI
  # driver are what let the image use current network and disk performance
  # instead of falling back to the oldest supported virtio path.
  image_guest_os_features = [
    "UEFI_COMPATIBLE",
    "VIRTIO_SCSI_MULTIQUEUE",
    "GVNIC",
  ]

  disk_size = var.disk_size
  disk_type = "pd-balanced"

  ssh_username = "packer"

  # The build instance needs no service account of its own: everything it
  # fetches comes from the public internet, and giving it one would put a
  # token on a machine whose disk becomes the published image.
  scopes = []

  # Project-wide SSH keys would be inherited by the build instance and could
  # end up in the image. Packer's own ephemeral key is enough.
  metadata = {
    block-project-ssh-keys = "true"
  }

  image_labels = {
    product        = "recall"
    recall-version = replace(var.recall_version, ".", "-")
    polign-version = replace(var.polign_version, ".", "-")
    architecture   = var.arch
    built-at       = local.timestamp
  }
}

build {
  name    = "recall"
  sources = ["source.googlecompute.recall"]

  # Fail before provisioning if the resolved credentials point somewhere other
  # than the producer project. The check runs on the build host through gcloud
  # rather than on the instance, so it costs nothing and names the mismatch.
  provisioner "shell-local" {
    inline = [
      "set -eu",
      "got=$(gcloud config get-value project 2>/dev/null || true)",
      "if [ -n \"$got\" ] && [ \"$got\" != '${var.project_id}' ]; then",
      "  echo \"active gcloud project $got is not the producer project ${var.project_id}\" >&2",
      "  echo \"run: gcloud config set project ${var.project_id}\" >&2",
      "  exit 1",
      "fi",
      "echo \"building into ${var.project_id}\"",
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

  post-processor "manifest" {
    output     = "manifest.json"
    strip_path = true
    custom_data = {
      recall_version = var.recall_version
      polign_version = var.polign_version
      arch           = var.arch
      project_id     = var.project_id
    }
  }
}
