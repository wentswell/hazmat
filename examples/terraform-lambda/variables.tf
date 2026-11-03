variable "region" {
  description = "The AWS region to deploy into."
  type        = string
}

variable "name" {
  description = "The name of the function. Also used for the role and the log group."
  type        = string
  default     = "hazmat"
}

variable "bucket_name" {
  description = "The name of the upload bucket to create. It must be globally unique."
  type        = string
}

variable "upload_prefix" {
  description = "The key prefix that hazmat may read from and write to. Each upload goes below it, for example uploads/<id>/source."
  type        = string
  default     = "uploads/"

  # Plain path segments only. The prefix goes into the IAM resource ARN, where
  # "*" or "?" would grant more than the prefix. No segment starts with ".", so
  # there is no "." or "..", which hazmat refuses in a key.
  validation {
    condition     = can(regex("^([A-Za-z0-9_-][A-Za-z0-9._-]*/)+$", var.upload_prefix))
    error_message = "upload_prefix must be one or more path segments of letters, digits, \".\", \"_\" or \"-\", each ending with \"/\", for example \"uploads/\"."
  }
}

variable "zip_path" {
  description = "The path to the release zip, for example hazmat_v0.3.1_lambda_arm64.zip."
  type        = string
}

variable "zip_sha256" {
  description = "The sha256 of the zip, copied from checksums.txt of the same release. The apply fails if the file does not match."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-f]{64}$", var.zip_sha256))
    error_message = "zip_sha256 must be 64 lowercase hex characters."
  }
}

variable "memory_size" {
  description = "Memory in MB. Lambda gives CPU in proportion to memory."
  type        = number
  default     = 3008
}

variable "decode_timeout_seconds" {
  description = "HAZMAT_DECODE_TIMEOUT_SECONDS: the time limit for all ffmpeg runs of one video."
  type        = number
  default     = 20

  # hazmat refuses to start with anything else.
  validation {
    condition     = var.decode_timeout_seconds > 0 && floor(var.decode_timeout_seconds) == var.decode_timeout_seconds
    error_message = "decode_timeout_seconds must be a positive whole number."
  }
}

variable "timeout_seconds" {
  description = "The function timeout. Must be at least decode_timeout_seconds + 5."
  type        = number
  default     = 30
}

variable "reserved_concurrency" {
  description = "The most copies of the function that can run at once, so a burst of uploads cannot use all the concurrency of the account."
  type        = number
  default     = 10
}

variable "log_retention_days" {
  description = "How long to keep the function logs."
  type        = number
  default     = 14
}

variable "environment" {
  description = "Extra HAZMAT_* variables, for example lower limits. See the Configuration section of the top-level README."
  type        = map(string)
  default     = {}
}
