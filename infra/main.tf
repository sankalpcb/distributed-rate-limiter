# Infrastructure for the benchmark.
#
# The point of putting this in Terraform is not the IaC line on a resume -- it
# is that `terraform destroy` is reliable teardown. Manual teardown is how a
# Memorystore instance survives forgotten for three weeks and eats the budget.

terraform {
  required_version = ">= 1.5"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

locals {
  # Memorystore lives on a private IP, so the service and the load generator
  # both need to be inside this network to reach it.
  network_name = "${var.name_prefix}-net"
}

resource "google_project_service" "required" {
  for_each = toset([
    "run.googleapis.com",
    "redis.googleapis.com",
    "compute.googleapis.com",
    "artifactregistry.googleapis.com",
    "cloudbuild.googleapis.com",
    "billingbudgets.googleapis.com",
  ])
  service            = each.key
  disable_on_destroy = false
}

# --- Network -----------------------------------------------------------------

resource "google_compute_network" "vpc" {
  name                    = local.network_name
  auto_create_subnetworks = false
  depends_on              = [google_project_service.required]
}

resource "google_compute_subnetwork" "subnet" {
  name          = "${var.name_prefix}-subnet"
  ip_cidr_range = "10.10.0.0/24"
  region        = var.region
  network       = google_compute_network.vpc.id
}

# SSH to the load generator. Scoped to var.ssh_source_ranges rather than
# 0.0.0.0/0 -- an open SSH port on a public IP is found by scanners in minutes.
resource "google_compute_firewall" "ssh" {
  name          = "${var.name_prefix}-allow-ssh"
  network       = google_compute_network.vpc.name
  source_ranges = var.ssh_source_ranges
  target_tags   = ["loadgen"]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# --- Redis -------------------------------------------------------------------

resource "google_redis_instance" "cache" {
  name           = "${var.name_prefix}-redis"
  tier           = "BASIC" # no replica: this is a benchmark, not production
  memory_size_gb = 1
  region         = var.region

  authorized_network = google_compute_network.vpc.id
  connect_mode       = "DIRECT_PEERING"
  redis_version      = "REDIS_7_0"

  depends_on = [google_project_service.required]
}

# --- Service -----------------------------------------------------------------

resource "google_artifact_registry_repository" "images" {
  location      = var.region
  repository_id = "${var.name_prefix}-images"
  format        = "DOCKER"
  depends_on    = [google_project_service.required]
}

resource "google_cloud_run_v2_service" "limiterd" {
  name     = var.service_name
  location = var.region

  # The benchmark drives this service's env vars and instance counts via
  # `gcloud run services update` (see bench/run.sh), so Terraform must not
  # fight the harness by reverting them on the next apply.
  lifecycle {
    ignore_changes = [
      template[0].containers[0].env,
      template[0].scaling,
    ]
  }

  template {
    scaling {
      min_instance_count = 0
      # The guardrail that actually protects the budget. The platform default
      # ceiling is 100; a runaway load generator against that ceiling is the
      # one plausible way this project spends real money.
      max_instance_count = var.max_instances
    }

    # Direct VPC egress rather than a Serverless VPC Access connector: same
    # private reachability to Memorystore, without the connector's hourly bill.
    vpc_access {
      network_interfaces {
        network    = google_compute_network.vpc.id
        subnetwork = google_compute_subnetwork.subnet.id
      }
      egress = "PRIVATE_RANGES_ONLY"
    }

    containers {
      image = var.image

      env {
        name  = "REDIS_ADDR"
        value = "${google_redis_instance.cache.host}:${google_redis_instance.cache.port}"
      }
      env {
        name  = "LIMITER_STRATEGY"
        value = "centralized"
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
      }

      startup_probe {
        http_get { path = "/healthz" }
        initial_delay_seconds = 2
        period_seconds        = 3
        failure_threshold     = 10
      }
    }
  }

  depends_on = [google_project_service.required]
}

# Public invocation: the load generator calls the service over its HTTPS URL.
# Same region, so the path stays inside Google's network.
resource "google_cloud_run_v2_service_iam_member" "public" {
  name     = google_cloud_run_v2_service.limiterd.name
  location = google_cloud_run_v2_service.limiterd.location
  role     = "roles/run.invoker"
  member   = "allUsers"
}

# --- Load generator ----------------------------------------------------------

resource "google_compute_instance" "loadgen" {
  name         = "${var.name_prefix}-loadgen"
  machine_type = var.loadgen_machine_type
  zone         = var.zone
  tags         = ["loadgen"]

  boot_disk {
    initialize_params {
      image = "debian-cloud/debian-12"
      size  = 20
    }
  }

  network_interface {
    network    = google_compute_network.vpc.id
    subnetwork = google_compute_subnetwork.subnet.id
    access_config {} # ephemeral public IP, for SSH and package installs
  }

  metadata_startup_script = <<-EOT
    #!/bin/bash
    set -eux
    apt-get update
    apt-get install -y git curl
    curl -sSL https://go.dev/dl/go1.26.0.linux-amd64.tar.gz -o /tmp/go.tgz
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
    echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile.d/go.sh
    # Default file descriptor limits cannot sustain tens of thousands of
    # concurrent sockets; without this the generator caps out long before the
    # service does and every measurement is really a measurement of ulimit.
    echo '* soft nofile 1048576' >> /etc/security/limits.conf
    echo '* hard nofile 1048576' >> /etc/security/limits.conf
  EOT

  depends_on = [google_project_service.required]
}

# --- Budget ------------------------------------------------------------------

# Note: this NOTIFIES, it does not cap. GCP has no hard spend limit short of
# wiring a budget through Pub/Sub to a function that disables billing, which is
# rejected here as fragile and capable of killing a project mid-run.
resource "google_billing_budget" "budget" {
  count = var.billing_account == "" ? 0 : 1

  billing_account = var.billing_account
  display_name    = "${var.name_prefix}-budget"

  budget_filter {
    projects = ["projects/${var.project_number}"]
  }

  amount {
    specified_amount {
      currency_code = "USD"
      units         = tostring(var.budget_amount)
    }
  }

  dynamic "threshold_rules" {
    for_each = var.budget_thresholds
    content {
      threshold_percent = threshold_rules.value
    }
  }

  depends_on = [google_project_service.required]
}
