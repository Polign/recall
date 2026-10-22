# Builds the Recall AMI listed on AWS Marketplace.
#
# The image carries the Recall and polign_db release binaries, the built-in
# embedding model, systemd units for both, and a first-boot script that
# resolves the store and generates the instance's own keys. Recall listens on
# :8080; the database behind it listens on localhost only. The image carries
# no credentials and no store configuration: a launch supplies the bucket, and
# the CloudFormation template that ships with the listing supplies the bucket,
# the instance role, and a VPC-scoped security group.
#
#   packer init .
#   AWS_PROFILE=polign packer build -var recall_version=v0.22.0 .
#
# The profile matters: the image must be built in the Marketplace seller
# account (see the seller_account variable), and the default profile is a
# different account.

packer {
  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = "~> 1.3"
    }
  }
}

variable "recall_version" {
  type        = string
  default     = "v0.22.0"
  description = "Recall release tag to install (must be a published, non-draft release)."
}

variable "polign_version" {
  type        = string
  default     = "v0.15.0"
  description = "polign_db release tag to install as the backend."
}

variable "seller_account" {
  type        = string
  default     = "298123941500"
  description = <<-EOT
    The AWS account the AMI must be built in. Marketplace ingests from the
    seller account, so an image built anywhere else cannot be submitted and
    has to be rebuilt. The build fails fast when the resolved credentials
    belong to a different account: pass AWS_PROFILE=polign (or set an empty
    string here to skip the check).
  EOT
}

variable "region" {
  type        = string
  default     = "us-east-1"
  description = "Build region. Marketplace scans the AMI here, so keep the primary build in us-east-1."
}

variable "arch" {
  type        = string
  default     = "arm64"
  description = "Target architecture: arm64 (Graviton) or amd64."

  validation {
    condition     = contains(["arm64", "amd64"], var.arch)
    error_message = "The arch variable must be arm64 or amd64."
  }
}

variable "instance_type" {
  type        = string
  default     = ""
  description = "Build instance type. Empty picks a small default for the architecture."
}

variable "volume_size" {
  type        = number
  default     = 30
  description = "Root volume size in GiB. Holds the local disk cache tier, so leave room above the cache budget."
}

variable "ami_users" {
  type        = list(string)
  default     = []
  description = <<-EOT
    Accounts the finished AMI is shared with. Marketplace ingestion does NOT
    work this way (it assumes an IAM role in the seller account instead), so
    this is only for sharing an image to another account you own, for example
    to test a build from a second account.
  EOT
}

variable "ami_regions" {
  type        = list(string)
  default     = []
  description = "Extra regions to copy the AMI into. Marketplace expects the regions the listing offers."
}

locals {
  # Amazon's AMI names use "x86_64", the release tarballs use "amd64".
  ami_arch      = var.arch == "arm64" ? "arm64" : "x86_64"
  instance_type = var.instance_type != "" ? var.instance_type : (var.arch == "arm64" ? "t4g.medium" : "t3.medium")
  timestamp     = formatdate("YYYYMMDDhhmmss", timestamp())
  name          = "recall-${replace(var.recall_version, ".", "-")}-${var.arch}-${local.timestamp}"
}

source "amazon-ebs" "recall" {
  region        = var.region
  instance_type = local.instance_type
  ssh_username  = "ec2-user"

  source_ami_filter {
    filters = {
      name                = "al2023-ami-2023.*-kernel-6.1-${local.ami_arch}"
      root-device-type    = "ebs"
      virtualization-type = "hvm"
      architecture        = local.ami_arch
    }
    owners      = ["amazon"]
    most_recent = true
  }

  ami_name        = local.name
  ami_description = "Recall ${var.recall_version} on polign_db ${var.polign_version} (${var.arch}) on Amazon Linux 2023"
  ami_users       = var.ami_users
  snapshot_users  = var.ami_users
  ami_regions     = var.ami_regions

  # Marketplace requires unencrypted AMIs, ENA, and IMDSv2-only instances.
  encrypt_boot = false
  ena_support  = true
  imds_support = "v2.0"

  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  launch_block_device_mappings {
    device_name           = "/dev/xvda"
    volume_size           = var.volume_size
    volume_type           = "gp3"
    delete_on_termination = true
    encrypted             = false
  }

  run_tags = {
    Name = "packer-${local.name}"
  }

  tags = {
    Name          = local.name
    Product       = "recall"
    RecallVersion = var.recall_version
    PolignVersion = var.polign_version
    Architecture  = var.arch
    BaseImage     = "{{ .SourceAMIName }}"
    BuiltAt       = local.timestamp
  }
}

build {
  name    = "recall"
  sources = ["source.amazon-ebs.recall"]

  # Fail before provisioning if these credentials are not the seller account.
  # The check runs on the build host through the AWS CLI rather than on the
  # instance, so it costs nothing and reports the mismatch by name.
  provisioner "shell-local" {
    inline = [
      "set -eu",
      "if [ -n '${var.seller_account}' ]; then",
      "  got=$(aws sts get-caller-identity --query Account --output text)",
      "  if [ \"$got\" != '${var.seller_account}' ]; then",
      "    echo \"account $got is not the Marketplace seller account ${var.seller_account}; re-run with AWS_PROFILE=polign\" >&2",
      "    exit 1",
      "  fi",
      "  echo \"seller account $got confirmed\"",
      "fi",
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
    }
  }
}
