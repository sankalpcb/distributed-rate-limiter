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

  # Some APIs -- billingbudgets among them -- refuse requests from user
  # Application Default Credentials unless a quota project is attached. Setting
  # it here rather than via `gcloud auth application-default set-quota-project`
  # keeps the fix inside the repo, so a fresh clone works without first
  # mutating the operator's global gcloud state.
  billing_project       = var.project_id
  user_project_override = true
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

  # The provider defaults this to true, which makes `terraform destroy` fail
  # partway through -- after the VM and registry are gone but while Redis and
  # the network remain. For a benchmark environment that is created and
  # destroyed every session, a teardown that half-completes is worse than no
  # protection at all: it leaves billable resources behind precisely when you
  # believe you have cleaned up.
  deletion_protection = false

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
        # Cloud Run constrains these together: 4 vCPU requires at least 2 GiB,
        # and 8 vCPU at least 4 GiB. A mismatch is rejected at deploy time, not
        # at plan time, so it surfaces as a failed rollout rather than a
        # terraform error.
        #
        # Note that raising cpu raises the burn rate proportionally while
        # replicas are pinned: 20 replicas x 4 vCPU is 80 vCPU billing for as
        # long as min_instance_count holds them up. See the scale-down in
        # bench/sweep.sh.
        limits = {
          cpu    = var.service_cpu
          memory = var.service_memory
        }
      }

      startup_probe {
        # /health, not /healthz -- the latter is reserved by Google's front end
        # on Cloud Run and never reaches the container.
        http_get { path = "/health" }
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

# The load generator drives `gcloud run services update` to sweep replica
# counts and strategies, so it needs credentials. A dedicated account rather
# than the default compute one: this VM is reachable over SSH and runs a load
# generator, and the blast radius of a compromise should be "can reconfigure
# one Cloud Run service", not "is a project Editor".
#
# Note that google_compute_instance attaches NO service account when the block
# is omitted -- unlike the console, which quietly attaches the default. A VM
# with no account gets no credentials at all, and every gcloud call on it fails.
resource "google_service_account" "loadgen" {
  account_id   = "${var.name_prefix}-loadgen"
  display_name = "Load generator for the rate limiter benchmark"
}

# run.developer covers services.get and services.update, which is all the
# harness does. run.admin would also allow deleting the service and editing
# its IAM policy, neither of which the benchmark needs.
resource "google_project_iam_member" "loadgen_run" {
  project = var.project_id
  role    = "roles/run.developer"
  member  = "serviceAccount:${google_service_account.loadgen.email}"
}

# Cloud Run resolves and validates the container image as the principal doing
# the deploy, not as the service's runtime identity. So the harness account
# needs to read the repository too -- without this, `gcloud run services
# update` fails on artifactregistry.repositories.downloadArtifacts even when
# the update itself is permitted. Scoped to this one repository.
resource "google_artifact_registry_repository_iam_member" "loadgen_pull" {
  location   = google_artifact_registry_repository.images.location
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.loadgen.email}"
}

# Updating a Cloud Run service requires actAs on the identity that service runs
# as. Scoped to that one account rather than granted project-wide.
resource "google_service_account_iam_member" "loadgen_actas_runtime" {
  service_account_id = "projects/${var.project_id}/serviceAccounts/${var.project_number}-compute@developer.gserviceaccount.com"
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.loadgen.email}"
}

resource "google_compute_instance" "loadgen" {
  name         = "${var.name_prefix}-loadgen"
  machine_type = var.loadgen_machine_type
  zone         = var.zone
  tags         = ["loadgen"]

  # Changing machine_type or service_account requires a stop/start. Allowing it
  # matters in practice: if `sweep.sh validate` shows the generator saturating
  # before the service does, resizing this VM is the fix, and that should be a
  # one-line change plus an apply rather than a manual console dance.
  allow_stopping_for_update = true

  service_account {
    email = google_service_account.loadgen.email
    # cloud-platform defers authorisation entirely to the IAM roles above.
    # The legacy per-scope model would silently block the Run API regardless
    # of what roles the account holds.
    scopes = ["cloud-platform"]
  }

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
    # make is not on the Debian 12 cloud image, and the benchmark harness is
    # driven through the Makefile.
    apt-get install -y git curl make
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
# Guardrails that would otherwise fail silently. A `check` block surfaces a
# warning on every plan and apply rather than an error, so a deliberate choice
# still goes through while an accidental omission stays visible.
check "budget_is_configured" {
  assert {
    condition = var.billing_account != ""
    error_message = join(" ", [
      "No billing_account set, so NO BUDGET ALERT will be created.",
      "You will have no notification as credits are consumed.",
      "Find it with: gcloud billing accounts list",
    ])
  }
}

check "ssh_is_not_open_to_the_world" {
  assert {
    condition = !contains(var.ssh_source_ranges, "0.0.0.0/0")
    error_message = join(" ", [
      "ssh_source_ranges includes 0.0.0.0/0.",
      "An open SSH port on a public IP is found by scanners within minutes.",
      "Set it to your own address: curl -s ifconfig.me",
    ])
  }
}

resource "google_billing_budget" "budget" {
  count = var.billing_account == "" ? 0 : 1

  billing_account = var.billing_account
  display_name    = "${var.name_prefix}-budget"

  budget_filter {
    projects = ["projects/${var.project_number}"]

    # Measure GROSS cost, before free credits are applied.
    #
    # The API default is INCLUDE_ALL_CREDITS, which measures net cost -- what
    # you actually pay. On a trial account that reads as zero for as long as
    # the credits last, so the budget stays silent through the entire credit
    # burn and only speaks up once real charges begin. That is the opposite of
    # what is wanted here: the whole point is to watch the credits drain.
    credit_types_treatment = var.budget_tracks_credits ? "EXCLUDE_ALL_CREDITS" : "INCLUDE_ALL_CREDITS"
  }

  amount {
    specified_amount {
      # The budget's currency must match the billing account's, or the API
      # rejects the request with a bare "invalid argument". Leave
      # budget_currency empty to inherit the account's currency, which is the
      # safe default. Check yours with:
      #   gcloud billing accounts describe ACCOUNT_ID --format='value(currencyCode)'
      currency_code = var.budget_currency != "" ? var.budget_currency : null
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
