# infra/bootstrap/variables.tf
# Inputs for the persistent stack.
variable "aws_region" {
  description = "AWS region for the ECR repository and the CI role."
  type        = string
  default     = "us-east-1"
}

# GitHub Actions OIDC tokens now carry immutable numeric IDs alongside the owner
# and repository names:
#
#   repo:Go-Santiago-Go@85260356/rag-api@1281557182:ref:refs/heads/main
#
# The `@` separator is safe because it cannot appear in a GitHub owner or
# repository name. GitHub applies this format to all repositories created after
# 2026-07-15, and to older ones that opt in. See
# https://github.blog/changelog/2026-04-23-immutable-subject-claims-for-github-actions-oidc-tokens/
#
# The trust policy therefore matches on the IDs and wildcards the names. That is
# the whole point of the immutable format: the IDs are globally unique and
# survive a rename, so a name-based policy is both weaker (a recycled name could
# be claimed by someone else) and more fragile (a rename silently breaks CD with
# `Not authorized to perform sts:AssumeRoleWithWebIdentity`).
#
# Look these up with:
#   gh api repos/OWNER/REPO --jq '{repo: .id, owner: .owner.id}'
variable "github_org_id" {
  description = "Numeric GitHub organization ID (Go-Santiago-Go). Immutable, so it survives an org rename."
  type        = string
  default     = "85260356"
}

variable "github_repo_id" {
  description = "Numeric GitHub repository ID (rag-api). Immutable, so it survives a repo rename."
  type        = string
  default     = "1281557182"
}
