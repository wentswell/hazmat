# The upload bucket. Versioning is required: every request names the
# source.version_id it read, so a second upload to the same key cannot change
# what a running request sees.
resource "aws_s3_bucket" "uploads" {
  bucket = var.bucket_name
}

resource "aws_s3_bucket_versioning" "uploads" {
  bucket = aws_s3_bucket.uploads.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_public_access_block" "uploads" {
  bucket = aws_s3_bucket.uploads.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "uploads" {
  bucket = aws_s3_bucket.uploads.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

# The log group is made here, with a retention, so the role does not need
# logs:CreateLogGroup.
resource "aws_cloudwatch_log_group" "hazmat" {
  name              = "/aws/lambda/${var.name}"
  retention_in_days = var.log_retention_days
}

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "hazmat" {
  name               = var.name
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

# Only what the worker does: read a version of an upload, write the outputs,
# and write its log. It never lists or deletes. The role is the real limit on
# the function, so keep it this small.
data "aws_iam_policy_document" "hazmat" {
  statement {
    actions   = ["s3:GetObject", "s3:GetObjectVersion", "s3:PutObject"]
    resources = ["${aws_s3_bucket.uploads.arn}/${var.upload_prefix}*"]
  }

  statement {
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.hazmat.arn}:*"]
  }
}

resource "aws_iam_role_policy" "hazmat" {
  name   = var.name
  role   = aws_iam_role.hazmat.id
  policy = data.aws_iam_policy_document.hazmat.json
}

resource "aws_lambda_function" "hazmat" {
  function_name = var.name
  role          = aws_iam_role.hazmat.arn

  filename         = var.zip_path
  source_code_hash = filebase64sha256(var.zip_path)
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  handler          = "bootstrap"

  memory_size                    = var.memory_size
  timeout                        = var.timeout_seconds
  reserved_concurrent_executions = var.reserved_concurrency

  environment {
    variables = merge(var.environment, {
      HAZMAT_ALLOWED_BUCKETS        = aws_s3_bucket.uploads.id
      HAZMAT_INPUT_PREFIXES         = var.upload_prefix
      HAZMAT_OUTPUT_PREFIXES        = var.upload_prefix
      HAZMAT_DECODE_TIMEOUT_SECONDS = tostring(var.decode_timeout_seconds)
    })
  }

  depends_on = [
    aws_cloudwatch_log_group.hazmat,
    aws_iam_role_policy.hazmat,
  ]

  lifecycle {
    precondition {
      condition     = filesha256(var.zip_path) == var.zip_sha256
      error_message = "The sha256 of zip_path does not match zip_sha256. Download the zip again and check it against checksums.txt."
    }

    precondition {
      condition     = var.timeout_seconds >= var.decode_timeout_seconds + 5
      error_message = "timeout_seconds must be at least decode_timeout_seconds + 5, so ffmpeg is stopped before Lambda stops the function."
    }
  }
}

# A policy for the service that calls hazmat. Attach it to that service's role.
data "aws_iam_policy_document" "invoke" {
  statement {
    actions   = ["lambda:InvokeFunction"]
    resources = [aws_lambda_function.hazmat.arn]
  }
}
