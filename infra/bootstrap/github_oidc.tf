# infra/bootstrap/github_oidc.tf
# Keyless CI auth: instead of storing a long-lived AWS access key in GitHub,
# we federate GitHub's OIDC identity provider and let a workflow in THIS repo
# assume a role scoped to exactly one action set (push to the ECR repo). The
# workflow trades a short-lived, signed GitHub token for temporary AWS creds
# via sts:AssumeRoleWithWebIdentity. Nothing to leak, nothing to rotate.

# ---------------------------------------------------------------------------
# The OIDC provider: reference the account's existing GitHub provider
# ---------------------------------------------------------------------------
# The GitHub OIDC provider is an account-level singleton (one per issuer URL)
# and already exists in this account, so we read it with a data source instead
# of creating it. This avoids ownership conflicts and, importantly, means
# `terraform destroy` will NOT remove a provider other stacks may rely on.
data "aws_iam_openid_connect_provider" "github" {
  url = "https://token.actions.githubusercontent.com"
}

# ---------------------------------------------------------------------------
# Trust policy: WHO may assume the CI role
# ---------------------------------------------------------------------------
data "aws_iam_policy_document" "github_assume" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"] # the OIDC-specific assume action

    principals {
      type        = "Federated"
      identifiers = [data.aws_iam_openid_connect_provider.github.arn]
    }

    # The token's audience must be STS (mirrors client_id_list on the provider).
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    # The token's subject must be a workflow in THIS repo. Without this
    # condition, any GitHub repo could assume the role. `:*` allows any branch;
    # tighten to `:ref:refs/heads/main` once the pipeline is proven.
    #
    # Matched against the immutable ID form of the claim, so the owner and repo
    # names are wildcards and the numeric IDs carry the security. See the
    # variables file for the claim format and why this is stronger than matching
    # on names.
    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:*@${var.github_org_id}/*@${var.github_repo_id}:*"]
    }
  }
}

resource "aws_iam_role" "github_ci" {
  name               = "go-rag-api-github-ci"
  assume_role_policy = data.aws_iam_policy_document.github_assume.json
  tags               = { Name = "go-rag-api-github-ci" }
}

# ---------------------------------------------------------------------------
# Permissions policy: WHAT the CI role may do (push to the one ECR repo)
# ---------------------------------------------------------------------------
data "aws_iam_policy_document" "github_ecr_push" {
  # `docker login`: GetAuthorizationToken is an account-level auth call and must
  # be granted on "*" (it is not tied to a specific repository).
  statement {
    sid       = "EcrAuth"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  # The push itself, scoped to only this repository's ARN.
  statement {
    sid = "EcrPush"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:InitiateLayerUpload",
      "ecr:UploadLayerPart",
      "ecr:CompleteLayerUpload",
      "ecr:PutImage",
      "ecr:BatchGetImage",
    ]
    resources = [aws_ecr_repository.app.arn]
  }
}

resource "aws_iam_role_policy" "github_ecr_push" {
  name   = "go-rag-api-github-ecr-push"
  role   = aws_iam_role.github_ci.id
  policy = data.aws_iam_policy_document.github_ecr_push.json
}
