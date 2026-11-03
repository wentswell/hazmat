output "function_name" {
  description = "The name of the function, for aws lambda invoke."
  value       = aws_lambda_function.hazmat.function_name
}

output "function_arn" {
  description = "The ARN of the function."
  value       = aws_lambda_function.hazmat.arn
}

output "bucket" {
  description = "The upload bucket."
  value       = aws_s3_bucket.uploads.id
}

output "invoke_policy_json" {
  description = "An IAM policy that lets your service invoke the function. Attach it to your service's role."
  value       = data.aws_iam_policy_document.invoke.json
}
