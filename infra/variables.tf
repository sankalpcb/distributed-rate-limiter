variable "project_id" {
  type        = string
  description = "GCP project ID."
}

variable "project_number" {
  type        = string
  description = "GCP project number, used by the budget filter. Find it with: gcloud projects describe PROJECT_ID --format='value(projectNumber)'"
  default     = ""
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "zone" {
  type        = string
  default     = "us-central1-a"
  description = "Zone for the load generator. Must be inside var.region -- cross-region load generation measures the network, not the limiter."
}

variable "name_prefix" {
  type    = string
  default = "drl"
}

variable "service_name" {
  type    = string
  default = "limiterd"
}

variable "image" {
  type        = string
  description = "Container image for limiterd, e.g. us-central1-docker.pkg.dev/PROJECT/drl-images/limiterd:latest"
}

variable "max_instances" {
  type        = number
  default     = 20
  description = "Hard ceiling on Cloud Run replicas. The budget guardrail that matters: it bounds what a runaway load generator can spend."
}

variable "loadgen_machine_type" {
  type        = string
  default     = "n2-standard-4"
  description = "Load generator size. If `sweep.sh validate` shows the generator saturating before the service does, increase this rather than trusting the numbers."
}

variable "ssh_source_ranges" {
  type        = list(string)
  description = "CIDRs allowed to SSH to the load generator. Set this to your own IP; do not use 0.0.0.0/0."
  default     = []
}

variable "billing_account" {
  type        = string
  default     = ""
  description = "Billing account ID for the budget alert. Leave empty to skip creating a budget."
}

variable "budget_amount" {
  type        = number
  default     = 50
  description = "Budget in USD. Alerts only -- this does not cap spending."
}

variable "budget_thresholds" {
  type    = list(number)
  default = [0.5, 1.0, 2.0] # $25, $50, $100 against a $50 budget
}
