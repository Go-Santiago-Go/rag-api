# infra/iam.tf
# The identity the ECS task assumes at runtime, plus a least-privilege policy
# granting exactly what the service calls: Bedrock (Titan embeddings + a Claude
# model) and read/write on the one S3 bucket. Nothing wider.

# Model IDs kept in one place. These mirror the constants in the Go service
# (internal/service/embed.go and generate.go); the policy must authorize the
# same models the code invokes, or calls fail AccessDenied.
locals {
  titan_model    = "amazon.titan-embed-text-v2:0"                # embeddings, called directly
  rerank_model   = "cohere.rerank-v3-5:0"                        # cross-encoder reranking, called directly
  claude_model   = "anthropic.claude-haiku-4-5-20251001-v1:0"    # the underlying foundation model
  claude_profile = "us.anthropic.claude-haiku-4-5-20251001-v1:0" # the cross-region inference profile we call

  # The "us." profile routes inference to any of these regions based on
  # capacity, so IAM must authorize the foundation model in each of them.
  bedrock_regions = ["us-east-1", "us-east-2", "us-west-2"]
}

# ---------------------------------------------------------------------------
# The task role: WHO the service is
# ---------------------------------------------------------------------------
# A trust policy answers "who is allowed to assume this role." Here, only the
# ECS tasks service, so only a running task can take on these permissions.
data "aws_iam_policy_document" "task_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "task" {
  name               = "go-rag-api-task"
  assume_role_policy = data.aws_iam_policy_document.task_assume.json
  tags               = { Name = "go-rag-api-task" }
}

# ---------------------------------------------------------------------------
# The permissions policy: WHAT the service may do
# ---------------------------------------------------------------------------
data "aws_iam_policy_document" "task_permissions" {

  # Titan embeddings: called directly (no inference profile), so it is pinned to
  # a single region and a single foundation-model ARN. This is the simple case.
  statement {
    sid       = "TitanEmbeddings"
    actions   = ["bedrock:InvokeModel"]
    resources = ["arn:aws:bedrock:${var.aws_region}::foundation-model/${local.titan_model}"]
  }

  # Cohere Rerank: like Titan, invoked directly rather than through an inference
  # profile, so one action against one foundation-model ARN in one region. No
  # wildcard in the resource and no bedrock:* in the action: the role can invoke
  # this specific model and nothing else in Bedrock.
  statement {
    sid       = "CohereRerank"
    actions   = ["bedrock:InvokeModel"]
    resources = ["arn:aws:bedrock:${var.aws_region}::foundation-model/${local.rerank_model}"]
  }

  # Claude generation via a cross-region inference profile. This is the subtle
  # case: authorizing the profile ARN alone is not enough. Bedrock forwards the
  # actual inference to whichever region has capacity, and IAM checks the
  # foundation-model ARN it routes TO. So we allow the profile ARN (carries our
  # account ID) AND the foundation-model ARN in every region it may route to.
  # Miss the routed regions and calls fail AccessDenied intermittently, under
  # load, in a way that passes every light test.
  statement {
    sid     = "ClaudeGeneration"
    actions = ["bedrock:InvokeModel"]
    resources = concat(
      ["arn:aws:bedrock:${var.aws_region}:${data.aws_caller_identity.current.account_id}:inference-profile/${local.claude_profile}"],
      [for r in local.bedrock_regions : "arn:aws:bedrock:${r}::foundation-model/${local.claude_model}"]
    )
  }

  # S3: read and write objects, but ONLY inside our bucket. The "/*" scopes this
  # to the bucket's objects; the role cannot touch any other bucket in the account.
  statement {
    sid       = "RawDocsBucket"
    actions   = ["s3:GetObject", "s3:PutObject"]
    resources = ["${aws_s3_bucket.docs.arn}/*"]
  }
}

resource "aws_iam_role_policy" "task" {
  name   = "go-rag-api-task-permissions"
  role   = aws_iam_role.task.id
  policy = data.aws_iam_policy_document.task_permissions.json
}
