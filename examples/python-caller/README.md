# Calling hazmat from your API (Python)

[`caller.py`](caller.py) is the other half of the
[Terraform example](../terraform-lambda): the code in your API that takes an
upload, hands it to hazmat, and acts on the answer. It's one short file with
plain functions, so you can lift the parts you need into whatever framework
you use.

## The flow

The point of hazmat is that your API never touches the uploaded bytes. Not
when they arrive, and not when they're processed:

```
 App                      Your API                    S3                hazmat
  |  "I want to upload"  ->  |                          |                   |
  |  <- media id + presigned POST                       |                   |
  |  ---------------- upload the file ----------------> |                   |
  |  "done"              ->  |  head: which version? -> |                   |
  |                          |  invoke(request) ------------------------->  |
  |                          |                          |  <- read version  |
  |                          |                          |  <- write JPEGs   |
  |                          |  <- ok / refused / error ------------------  |
  |  <- 200 / 400 / 503      |                          |                   |
```

1. **`start_upload`**: the API makes a media id and a presigned POST for
   `uploads/<id>/source`. The API picks the key, so the app can't write
   anywhere else, and the size limit is part of the signature, so S3 rejects a
   file that's too big.
2. **The app uploads** straight to S3.
3. **`finish_upload`**: the app says it's done. The API looks up the version
   that's there now and passes that exact `version_id` to hazmat. If someone
   uploads to the same key again, it doesn't change what gets processed.

## What to do with each answer

`process()` turns every answer from hazmat into one of four outcomes:

| hazmat says | `caller.py` does | Your API returns |
|---|---|---|
| `status: ok` | returns the response | 200, and stores the output keys |
| `status: refused` | raises `Refused(code, detail)` | 400. Show the user. Never retry: the same file always gets the same answer. |
| `StorageError`, `WorkerError`, a Lambda timeout, throttling | raises `TryLater` | 503, or retry from a queue |
| `InvalidRequestError` | raises `RuntimeError` | 500. The request is wrong, so it's a bug in your code. Fix it. |

`to_record()` then picks out what's worth storing: the kind, the size, and the
keys to serve. For a video, it also picks a poster frame with `choose_poster()`,
using the brightness and sharpness that hazmat measures for each frame.

## Run it against your deployment

After `terraform apply` in [`../terraform-lambda`](../terraform-lambda):

```sh
export HAZMAT_BUCKET=$(terraform -chdir=../terraform-lambda output -raw bucket)
export HAZMAT_FUNCTION=$(terraform -chdir=../terraform-lambda output -raw function_name)
uv run caller.py photo.jpg
```

([uv](https://docs.astral.sh/uv/) reads the dependencies from the top of the
file. Without it: `pip install boto3 requests` and `python caller.py photo.jpg`.)

The AWS credentials you run it with need `s3:PutObject` and `s3:GetObject` on
the upload prefix and `lambda:InvokeFunction` on the function. That's the same
access your API will need.

You'll see something like this:

```
1. API made upload slot 121dfc9f0eac487185933ab342177aa9
2. App uploaded photo.jpg
3. Processed. Store this:
{
  "media_id": "121dfc9f0eac487185933ab342177aa9",
  "kind": "photo",
  "width": 800,
  "height": 1200,
  "keys": {
    "image": "uploads/121dfc9f0eac487185933ab342177aa9/image.jpg",
    "thumb": "uploads/121dfc9f0eac487185933ab342177aa9/thumb.jpg"
  },
  "location_removed": true
}
```

Try a text file or an HEVC video too, to see a refusal.

## Run it with no AWS account

The repository's local test stack runs the real worker in the Lambda runtime
image, with a local S3 double. `caller.py` works against it unchanged. You need
Go and Docker. From the root of the repository:

```sh
make ffmpeg
CGO_ENABLED=0 GOOS=linux GOARCH=$(docker version --format '{{.Server.Arch}}') \
  go build -tags lambda.norpc -o dist/local/bootstrap ./cmd/hazmat
docker compose -f test/local/compose.yaml up -d

unset AWS_PROFILE
printf '[default]\ns3 =\n    addressing_style = path\n' > /tmp/hazmat-aws-config
export AWS_CONFIG_FILE=/tmp/hazmat-aws-config AWS_DEFAULT_REGION=us-east-2 \
  AWS_ACCESS_KEY_ID=local-access-key AWS_SECRET_ACCESS_KEY=local-secret-key \
  AWS_ENDPOINT_URL_S3=http://127.0.0.1:19000 \
  AWS_ENDPOINT_URL_LAMBDA=http://127.0.0.1:19001 \
  HAZMAT_BUCKET=cdn HAZMAT_FUNCTION=function

uv run --with boto3 python -c "import boto3; s3 = boto3.client('s3'); \
  s3.create_bucket(Bucket='cdn', CreateBucketConfiguration={'LocationConstraint': 'us-east-2'}); \
  s3.put_bucket_versioning(Bucket='cdn', VersioningConfiguration={'Status': 'Enabled'})"

uv run examples/python-caller/caller.py photo.jpg
uv run examples/python-caller/caller.py test/local/testdata/good.mp4

docker compose -f test/local/compose.yaml down -v
```

The local stack sets low limits (1 MiB photos, 2 MiB videos), so use small
files.

## Things to know in production

- **Never serve the source.** `uploads/<id>/source` still has the original
  bytes, metadata and all. Serve only the keys in the record. Consider a
  lifecycle rule that deletes sources after a few days.
- **Retries are safe.** Processing the same version again writes the same
  output keys, so a retry after a timeout does no harm.
- **The invoke is synchronous.** A photo is quick, and a video can take
  several seconds. If you don't want a request to wait that long,
  call `finish_upload` from a background job and let the app poll or get a push.
- **Set the read timeout above the function timeout.** `caller.py` sets 60
  seconds for a 30 second function. If it's shorter, the client gives up while
  hazmat is still working.
- **hazmat doesn't judge content.** If you moderate uploads, send the clean
  outputs (the photo, or the video frames) to your moderation step after
  `status: ok`. They're JPEGs that hazmat encoded itself, with no metadata.
