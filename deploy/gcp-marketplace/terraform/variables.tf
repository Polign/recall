variable "project_id" {
  type        = string
  description = "Project to deploy into."
}

variable "zone" {
  type        = string
  default     = "us-central1-a"
  description = "Zone for the instance. Put it in the same region as the bucket: the database reads from the bucket on every uncached query, and a cross-region read pays both latency and network egress."
}

variable "name" {
  type        = string
  default     = "recall"
  description = "Name prefix for every resource this module creates."
}

variable "image" {
  type        = string
  default     = ""
  description = <<-EOT
    Self link of the Recall image to launch, for example
    projects/<producer-project>/global/images/recall-v0-22-0-amd64-20261022000000

    Leave empty to track the published image family instead, which is what a
    Marketplace deployment does. A listing supplies this value.
  EOT
}

variable "image_family" {
  type        = string
  default     = "recall-amd64"
  description = "Image family used when var.image is empty."
}

variable "image_project" {
  type        = string
  default     = ""
  description = "Project holding the image. Empty means var.project_id, which is only right when you built the image yourself; a Marketplace launch points at the producer project."
}

variable "machine_type" {
  type        = string
  default     = "n2-standard-2"
  description = <<-EOT
    Machine type. The built-in embedder runs on the CPU, and the database
    keeps its hot tier in memory. Network egress caps scale with vCPU count on
    Google Cloud, so very small types throttle on bandwidth first.
  EOT
}

variable "boot_disk_gb" {
  type        = number
  default     = 100
  description = "Boot disk size in GB. It holds the local disk cache tier, so keep it comfortably above disk_cache_bytes."

  validation {
    condition     = var.boot_disk_gb >= 30
    error_message = "The boot disk must be at least 30 GB: the image alone plus the default cache budget does not fit below that."
  }
}

variable "disk_cache_bytes" {
  type        = number
  default     = 10737418240
  description = "Byte budget for the local disk cache under /var/cache/polign (the database cache)."
}

variable "bucket_name" {
  type        = string
  default     = ""
  description = "Existing bucket to store into. Leave empty to have this module create one. polign_db never creates its own bucket, so one of the two has to happen."
}

variable "bucket_location" {
  type        = string
  default     = "US"
  description = "Location for a bucket this module creates. Ignored when bucket_name is set."
}

variable "store_prefix" {
  type        = string
  default     = "recall"
  description = "Key prefix inside the bucket."
}

variable "network" {
  type        = string
  default     = "default"
  description = "Network to attach to."
}

variable "subnetwork" {
  type        = string
  default     = "default"
  description = "Subnetwork to attach to. It must have Private Google Access turned on unless you set assign_public_ip; the module checks this before creating the instance."
}

variable "allowed_cidrs" {
  type        = list(string)
  default     = []
  description = <<-EOT
    Source ranges allowed to reach Recall on port 8080. Use your VPC range.

    Every request also needs a Recall key; this rule is an independent
    boundary that keeps memory private. The module refuses 0.0.0.0/0.
  EOT

  validation {
    condition     = length(var.allowed_cidrs) > 0
    error_message = "Name at least one source range. There is no safe default, so this module will not guess one."
  }

  validation {
    condition     = !contains(var.allowed_cidrs, "0.0.0.0/0")
    error_message = "0.0.0.0/0 is refused. Key authentication does not make a memory server safe to expose directly to the internet. Use your VPC range."
  }
}

variable "assign_public_ip" {
  type        = bool
  default     = false
  description = <<-EOT
    Give the instance an external IP.

    The default is false, which is both the safer and the cheaper option: the
    instance reaches Cloud Storage over Private Google Access and you reach the
    instance over an IAP tunnel, so nothing is exposed and there is no egress
    through a NAT. Set it to true only if the subnet has no Private Google
    Access and you cannot turn it on. An external IP does not by itself expose
    Recall, the firewall rule still governs that, but it is one less
    layer.
  EOT
}

variable "embed_model" {
  type        = string
  default     = "local:all-minilm-l6-v2"
  description = "Embedder for search by meaning. The default runs inside the image with no API key; \"lexical\" is word search only. A hosted embedder such as openai:text-embedding-3-small@512 also needs its API key in /etc/recall/recall.env. Choose before the first write."
}

variable "extra_args" {
  type        = string
  default     = ""
  description = "Appended verbatim to the recall serve command line, word-split."
}

variable "labels" {
  type        = map(string)
  default     = {}
  description = "Labels applied to the instance and to a bucket this module creates."
}
