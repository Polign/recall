# Recall agent memory on a single Compute Engine instance, with polign_db as
# its store on the same instance, the Cloud Storage bucket that backs it, a
# service account scoped to that bucket, and a firewall rule scoped to a range
# you name. Recall listens on port 8080; the database listens on localhost only.
#
# What is deliberately not here: a key, a secret, or a credential of any kind.
# The instance reaches its bucket as the attached service account, and
# generates its own Recall keys on first boot.

terraform {
  required_version = ">= 1.3"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 4.50"
    }
  }
}

# Configured from the module's own variables so this directory works as a root
# module: `terraform apply -var project_id=... -var 'allowed_cidrs=[...]'` is
# all a buyer needs. Credentials come from the ambient environment, which is
# gcloud application-default credentials locally and the deployment service
# account in a Marketplace launch.
provider "google" {
  project = var.project_id
  zone    = var.zone
  region  = local.region
}

locals {
  # us-central1-a -> us-central1. Trimming the zone suffix avoids asking for
  # the region twice and getting a mismatched pair.
  region        = substr(var.zone, 0, length(var.zone) - 2)
  bucket        = var.bucket_name != "" ? var.bucket_name : google_storage_bucket.recall[0].name
  image_project = var.image_project != "" ? var.image_project : var.project_id
  store         = "gcs://${local.bucket}/${var.store_prefix}"
  network_tag   = "${var.name}-server"
}

# The bucket. Created only when the caller did not name one, and never deleted
# with the stack: force_destroy stays false so a `terraform destroy` cannot
# take the data with it.
resource "google_storage_bucket" "recall" {
  count = var.bucket_name == "" ? 1 : 0

  project  = var.project_id
  name     = "${var.name}-${var.project_id}"
  location = var.bucket_location
  labels   = var.labels

  # IAM only. Per-object ACLs would let an object carry a grant the bucket
  # policy does not show, which is exactly the kind of access that goes
  # unnoticed in a review.
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  force_destroy = false
}

# The instance's identity. It exists so the grant below can be scoped to this
# one bucket instead of the project-wide default compute service account,
# which is an editor on everything.
resource "google_service_account" "recall" {
  project      = var.project_id
  account_id   = "${var.name}-sa"
  display_name = "Recall server (${var.name})"
}

# The only grant the instance gets. objectAdmin covers read, write, and
# delete on the objects, which the compaction and retention paths need; it
# does not include deleting the bucket itself.
resource "google_storage_bucket_iam_member" "recall" {
  bucket = local.bucket
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.recall.email}"
}

# Reading back the subnet lets the instance's precondition below fail at plan
# time, with a message, instead of booting a VM that cannot reach its bucket.
data "google_compute_subnetwork" "recall" {
  project = var.project_id
  name    = var.subnetwork
  region  = local.region
}

resource "google_compute_instance" "recall" {
  project      = var.project_id
  zone         = var.zone
  name         = var.name
  machine_type = var.machine_type
  tags         = [local.network_tag]
  labels       = var.labels

  boot_disk {
    initialize_params {
      image = var.image != "" ? var.image : "projects/${local.image_project}/global/images/family/${var.image_family}"
      size  = var.boot_disk_gb
      type  = "pd-balanced"
    }
  }

  network_interface {
    network    = var.network
    subnetwork = var.subnetwork

    # An access_config block, even an empty one, is what assigns an external
    # IP. No block means no external IP.
    dynamic "access_config" {
      for_each = var.assign_public_ip ? [1] : []
      content {}
    }
  }

  service_account {
    email = google_service_account.recall.email

    # A scope is a ceiling on what any token from this VM can do; the IAM
    # grant above is what actually decides access. cloud-platform is the
    # current guidance: narrowing the scope here does not add security
    # beyond the single role the service account holds, and a narrower scope
    # is a common cause of confusing permission errors.
    scopes = ["https://www.googleapis.com/auth/cloud-platform"]
  }

  # The image reads all of these on first boot. There is no bucket name and no
  # credential baked into the image, so this is where the deployment becomes
  # a specific database.
  metadata = {
    recall-store = local.store

    # OS Login puts SSH access under IAM instead of under metadata keys, so
    # revoking a person's access is an IAM change rather than a hunt for a
    # public key.
    enable-oslogin = "TRUE"

    # Settings first boot would otherwise take from its defaults file. A
    # value written to /etc/recall/recall.env on the VM still wins.
    recall-embed-model      = var.embed_model
    recall-disk-cache-bytes = tostring(var.disk_cache_bytes)
    recall-extra-args       = var.extra_args
  }

  # Shielded VM. The image is built UEFI_COMPATIBLE, so this costs nothing and
  # means the boot chain is measured and verified.
  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  allow_stopping_for_update = true

  lifecycle {
    precondition {
      condition     = var.assign_public_ip || data.google_compute_subnetwork.recall.private_ip_google_access
      error_message = "Subnet ${var.subnetwork} does not have Private Google Access, and the instance has no external IP, so it could not reach Cloud Storage. Turn it on (gcloud compute networks subnets update ${var.subnetwork} --region ${local.region} --enable-private-ip-google-access) or set assign_public_ip = true."
    }
  }

  depends_on = [google_storage_bucket_iam_member.recall]
}

# The network boundary. Every request also needs a Recall key; this rule keeps
# the port itself private.
resource "google_compute_firewall" "recall" {
  project     = var.project_id
  name        = "${var.name}-allow-clients"
  network     = var.network
  description = "Clients allowed to reach Recall. Keep this scoped to your VPC."

  direction     = "INGRESS"
  source_ranges = var.allowed_cidrs
  target_tags   = [local.network_tag]

  allow {
    protocol = "tcp"
    ports    = ["8080"]
  }
}

# SSH over an IAP tunnel, from Google's fixed forwarding range only. This is
# what replaces opening port 22: no external IP, no bastion, and IAM decides
# who may open the tunnel.
resource "google_compute_firewall" "iap_ssh" {
  project     = var.project_id
  name        = "${var.name}-allow-iap-ssh"
  network     = var.network
  description = "SSH from the IAP forwarding range, so operators can reach the instance without a public IP."

  direction     = "INGRESS"
  source_ranges = ["35.235.240.0/20"]
  target_tags   = [local.network_tag]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}
