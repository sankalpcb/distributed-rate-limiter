output "service_url" {
  value       = google_cloud_run_v2_service.limiterd.uri
  description = "Base URL for limiterd. Pass to loadgen with -target."
}

output "redis_host" {
  value       = "${google_redis_instance.cache.host}:${google_redis_instance.cache.port}"
  description = "Private endpoint for Memorystore. Only reachable from inside the VPC."
}

output "loadgen_ssh" {
  value       = "gcloud compute ssh ${google_compute_instance.loadgen.name} --zone ${var.zone}"
  description = "Run the benchmark from here, never from a laptop."
}

output "artifact_repo" {
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
  description = "Push the limiterd image here."
}

output "teardown_reminder" {
  value = "When the session ends: make infra-down"
}
