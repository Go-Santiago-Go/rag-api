# infra/bootstrap/variables.tf
# Inputs for the persistent stack.
variable "aws_region" {
  description = "AWS region for the ECR repository and the CI role."
  type        = string
  default     = "us-east-1"
}

variable "github_repo" {
  description = "GitHub repository (owner/name) allowed to assume the CI role via OIDC. Case-sensitive: matched against the token's `sub` claim, so it must match the repo exactly as GitHub stores it. Renaming the repository on GitHub changes that claim and breaks deploys with 'Not authorized to perform sts:AssumeRoleWithWebIdentity' until this is updated and bootstrap is re-applied."
  type        = string
  default     = "Go-Santiago-Go/rag-api"
}
