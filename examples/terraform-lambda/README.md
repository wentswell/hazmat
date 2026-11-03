# Deploy hazmat on AWS Lambda with Terraform

This example puts hazmat in your AWS account, set up the way the top-level
README recommends. It creates:

- **An upload bucket** with versioning on and all public access blocked.
- **The Lambda function** from a release zip: `provided.al2023` on arm64, 3008
  MB of memory, a 30 second timeout, and a cap on concurrency.
- **An IAM role** that can only read and write objects below one prefix of that
  bucket and write its own log. It can't list or delete anything.
- **A log group** with a retention period.
- **An invoke policy** (an output) for the service that calls hazmat.

It's a starting point, not a module. Copy it into your own infrastructure and
change what you need.

## Before you start

You need Terraform 1.5 or newer, AWS credentials for the account you deploy to,
and the [GitHub CLI](https://cli.github.com/) (or a browser) to download a
release.

## 1. Download a release and check it

```sh
gh release download v0.3.1 --repo wentswell/hazmat \
  --pattern '*.zip' --pattern checksums.txt
shasum -a 256 -c checksums.txt --ignore-missing
```

You should see `hazmat_v0.3.1_lambda_arm64.zip: OK`. Pick the newest tag from
the [releases page](https://github.com/wentswell/hazmat/releases).

## 2. Set your variables

Create `terraform.tfvars`. It's in `.gitignore`.

```hcl
region      = "us-east-1"
bucket_name = "my-app-uploads-example"   # must be globally unique

zip_path   = "hazmat_v0.3.1_lambda_arm64.zip"
zip_sha256 = "3d62a2effc1eb1c5fd4f7c25b71a13c9100c1c33dd7b093db40d86da228cba82"  # from checksums.txt

# Optional: lower the limits to what your app needs.
environment = {
  HAZMAT_MAX_PHOTO_BYTES   = "10MiB"
  HAZMAT_MAX_VIDEO_SECONDS = "30"
}
```

`zip_sha256` is a second check: Terraform refuses to deploy if the file at
`zip_path` doesn't match it. Copy it from `checksums.txt`, not from the zip you
downloaded, so a swapped file can't vouch for itself.

`variables.tf` lists everything else you can change, with defaults.

## 3. Deploy

```sh
terraform init
terraform apply
```

## 4. Try it

Upload a photo, and note the `VersionId` that comes back:

```sh
BUCKET=$(terraform output -raw bucket)
aws s3api put-object --bucket "$BUCKET" --key uploads/test1/source --body photo.jpg
```

Write a request. Put your bucket name and the `VersionId` in it:

```json
{
  "v": 1,
  "request_id": "test1",
  "source": {"bucket": "my-app-uploads-example", "key": "uploads/test1/source", "version_id": "PASTE-VERSION-ID"},
  "output": {"bucket": "my-app-uploads-example", "prefix": "uploads/test1/"},
  "expect": "photo",
  "photo": {"renditions": [{"name": "image", "max_edge": 2048, "quality": 85}]}
}
```

Invoke the function and read the response:

```sh
aws lambda invoke --function-name "$(terraform output -raw function_name)" \
  --cli-binary-format raw-in-base64-out --payload file://request.json response.json
cat response.json
```

You should see `"status": "ok"`, and the clean photo is at
`uploads/test1/image.jpg` in the bucket. If the photo had a location in it, the
response says `"had_gps": true`, and the new JPEG doesn't have it.

## 5. Let your service call it

Attach the `invoke_policy_json` output to the IAM role of the service that
calls hazmat:

```sh
terraform output -raw invoke_policy_json
```

Your service invokes the function synchronously (`RequestResponse`) and handles
each result as the "How to handle each result" table of the
[top-level README](../../README.md#how-to-handle-each-result) says.

Your service also needs to put uploads into the bucket below the prefix (or
hand out presigned upload URLs for it) and read the outputs. This example
doesn't grant that, because it depends on your app.

## Things to know

- **Reserved concurrency on a new account.** A new AWS account can have a total
  concurrency limit as low as 10, and AWS always keeps some of it unreserved.
  If the apply fails on `reserved_concurrent_executions`, set
  `reserved_concurrency` lower or ask AWS for a higher limit.
- **Using a bucket you already have.** Delete the bucket resources from
  `main.tf` and point the role, the environment, and `bucket_name` at your
  bucket. It must have versioning on.
- **Old versions add up.** With versioning, an overwritten or deleted object
  keeps its old version. Add a lifecycle rule that expires noncurrent versions
  if you don't want to keep them.
- **Updating hazmat.** Download the new release, update `zip_path` and
  `zip_sha256`, and apply again.
- **Tearing it down.** `terraform destroy` can't delete a bucket that still
  holds objects or old versions. Empty it first.
