# infra/s3.tf
# The raw-document bucket, provisioned for storing original uploaded files.
# Nothing writes to it yet: /ingest takes text in the request body and persists
# only the resulting chunks and vectors to pgvector. The bucket and its task-role
# grant exist so raw-file upload can be added without another infra change.
# Named with the account ID so it is globally unique without a manual suffix.

# Read-only lookup of the current account (used here for the bucket name and in
# iam.tf for the Bedrock inference-profile ARN).
data "aws_caller_identity" "current" {}

resource "aws_s3_bucket" "docs" {
  bucket = "go-rag-api-${data.aws_caller_identity.current.account_id}"

  # force_destroy lets `terraform destroy` delete the bucket even with objects
  # still inside. Correct for a throwaway demo ONLY; never on real data.
  force_destroy = true

  tags = { Name = "go-rag-api-docs" }
}

# Belt-and-suspenders: explicitly block every path to making this bucket public.
# S3 blocks public access by default now, but declaring it makes the intent
# auditable and immune to a later careless ACL or policy.
resource "aws_s3_bucket_public_access_block" "docs" {
  bucket                  = aws_s3_bucket.docs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}
