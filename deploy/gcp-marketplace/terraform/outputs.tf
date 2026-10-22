output "instance_name" {
  value       = google_compute_instance.recall.name
  description = "Name of the instance, and the node id the database reports in its logs."
}

output "internal_ip" {
  value       = google_compute_instance.recall.network_interface[0].network_ip
  description = "Address clients inside the VPC connect to."
}

output "recall_endpoint" {
  value       = "http://${google_compute_instance.recall.network_interface[0].network_ip}:8080"
  description = "Base URL for Recall's HTTP API (/v1/...) and MCP endpoint (/mcp), reachable from the ranges named in allowed_cidrs."
}

output "store" {
  value       = local.store
  description = "The store spec the instance was configured with."
}

output "bucket" {
  value       = local.bucket
  description = "Bucket backing Recall. It is not deleted when this stack is destroyed."
}

output "service_account" {
  value       = google_service_account.recall.email
  description = "The instance's identity. Grant this on a bucket in another project to let the instance store there."
}

output "ssh_command" {
  value       = "gcloud compute ssh ${google_compute_instance.recall.name} --zone ${var.zone} --project ${var.project_id} --tunnel-through-iap"
  description = "How to reach the instance without a public IP."
}

output "admin_key_command" {
  value       = "gcloud compute ssh ${google_compute_instance.recall.name} --zone ${var.zone} --project ${var.project_id} --tunnel-through-iap --command 'sudo cat /etc/recall/admin-key'"
  description = "Prints the instance's first Recall admin key, which mints namespace keys for your applications (POST /v1/keys)."
}
