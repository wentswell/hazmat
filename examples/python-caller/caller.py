# /// script
# requires-python = ">=3.10"
# dependencies = ["boto3", "requests"]
# ///
"""The API side of hazmat: how a service takes an upload without touching it.

The flow has three steps, and the API never handles the bytes of the file:

1. start_upload: the API picks the key and gives the app a presigned upload.
2. The app uploads the file straight to S3.
3. finish_upload: the app says "done". The API pins the version it got, asks
   hazmat to process it, and acts on the answer.

Run it against the Terraform example to see the whole round trip:

    export HAZMAT_BUCKET=$(terraform -chdir=../terraform-lambda output -raw bucket)
    uv run caller.py photo.jpg
"""

import json
import os
import sys
import uuid

import boto3
import requests
from botocore.config import Config
from botocore.exceptions import ClientError

BUCKET = os.environ["HAZMAT_BUCKET"]
FUNCTION = os.environ.get("HAZMAT_FUNCTION", "hazmat")
PREFIX = "uploads/"  # The same prefix the function is allowed to use.

# The largest file the app may upload: the larger of HAZMAT_MAX_PHOTO_BYTES and
# HAZMAT_MAX_VIDEO_BYTES. hazmat still checks the real limit for each kind.
MAX_UPLOAD_BYTES = 50 * 1024 * 1024

s3 = boto3.client("s3")
# The read timeout must be longer than the function timeout (30 s in the
# example), or the client gives up while hazmat is still working. If botocore
# retries an invoke, that's fine: the same request writes the same outputs.
lambda_client = boto3.client("lambda", config=Config(read_timeout=60))


class Refused(Exception):
    """The file is the problem. Tell the user (HTTP 400). Never retry."""

    def __init__(self, code: str, detail: str):
        super().__init__(f"{code}: {detail}")
        self.code = code
        self.detail = detail


class TryLater(Exception):
    """Something temporary failed. Return HTTP 503, or retry from a queue."""


# Step 1 ----------------------------------------------------------------------


def start_upload() -> dict:
    """Make a new media id and a presigned POST the app can upload to.

    The API chooses the key, so the app can't write anywhere else. The size
    limit is part of the signature, so S3 itself rejects a file that's too big.
    """
    media_id = uuid.uuid4().hex
    post = s3.generate_presigned_post(
        Bucket=BUCKET,
        Key=f"{PREFIX}{media_id}/source",
        Conditions=[["content-length-range", 1, MAX_UPLOAD_BYTES]],
        ExpiresIn=600,
    )
    return {"media_id": media_id, "upload": post}


# Step 3 ----------------------------------------------------------------------


def finish_upload(media_id: str) -> dict:
    """Process the upload and return what the API should store about it."""
    # Pin the version that is there now. hazmat reads exactly this version, so
    # if the app (or anyone) uploads to the same key again, it changes nothing.
    try:
        head = s3.head_object(Bucket=BUCKET, Key=f"{PREFIX}{media_id}/source")
    except ClientError as e:
        # A missing key is a 404 only if the API's role has s3:ListBucket;
        # without it, S3 answers 403.
        if e.response["Error"]["Code"] in ("404", "NoSuchKey"):
            # Not a hazmat code: this one is the API's own.
            raise Refused("not_uploaded", "Nothing was uploaded for this id.") from e
        raise TryLater() from e

    response = process(media_id, head["VersionId"])
    return to_record(media_id, response)


def process(media_id: str, version_id: str) -> dict:
    """Invoke hazmat for one upload, and turn its answer into a result or an
    exception."""
    request = {
        "v": 1,
        "request_id": media_id,
        "source": {
            "bucket": BUCKET,
            "key": f"{PREFIX}{media_id}/source",
            "version_id": version_id,
        },
        "output": {"bucket": BUCKET, "prefix": f"{PREFIX}{media_id}/"},
        # "any" accepts a photo or a video, so both option blocks are required.
        "expect": "any",
        "photo": {
            "renditions": [
                {"name": "image", "max_edge": 2048, "quality": 85},
                {"name": "thumb", "max_edge": 400, "quality": 80},
            ]
        },
        "video": {
            "max_seconds": 30,
            "frames": {"count": 5},
            "frame_edge": 1280,
            "quality": 85,
        },
    }

    try:
        result = lambda_client.invoke(
            FunctionName=FUNCTION,
            InvocationType="RequestResponse",
            Payload=json.dumps(request).encode(),
        )
        payload = json.loads(result["Payload"].read())
    except Exception as e:  # Throttled, network down, a cut-off response.
        raise TryLater() from e

    # A function error: hazmat (or Lambda) failed, not the file. Lambda marks
    # it with FunctionError. The local Lambda emulator doesn't, so also look
    # for errorType, which a hazmat response never has.
    if "FunctionError" in result or "errorType" in payload:
        error_type = payload.get("errorType")
        if error_type == "InvalidRequestError":
            # The request above is wrong. That's a bug in this code: retrying
            # won't help, so let it surface as a 500 and get fixed.
            raise RuntimeError(f"bad hazmat request: {payload.get('errorMessage')}")
        # StorageError, WorkerError, a Lambda timeout, or anything else:
        # all temporary.
        raise TryLater()

    if payload["status"] == "refused":
        refusal = payload["refusal"]
        raise Refused(refusal["code"], refusal["detail"])

    return payload  # status == "ok"


def to_record(media_id: str, response: dict) -> dict:
    """Pick out what the API keeps in its database."""
    media = response["media"]
    outputs = {o["name"]: o for o in response["outputs"]}

    record = {
        "media_id": media_id,
        "kind": media["kind"],
        "width": media["width"],
        "height": media["height"],
        # Serve these keys. Never serve the source: it still has the original
        # bytes, metadata and all.
        "keys": {name: o["key"] for name, o in outputs.items()},
        # Optional: tell the user their location was removed.
        "location_removed": media.get("had_gps", False),
    }

    if media["kind"] == "video":
        record["duration_s"] = media["duration_s"]
        record["poster"] = choose_poster(response["outputs"])

    return record


def choose_poster(frames: list[dict]) -> str:
    """Pick the sharpest frame that isn't almost black."""
    not_dark = [f for f in frames if f["luma"] > 16] or frames
    return max(not_dark, key=lambda f: f["sharpness"])["key"]


# A demo of the round trip ------------------------------------------------------


def main(path: str) -> None:
    started = start_upload()
    print("1. API made upload slot", started["media_id"])

    # The app does this part, straight to S3. The API never sees the bytes.
    upload = started["upload"]
    with open(path, "rb") as f:
        r = requests.post(upload["url"], data=upload["fields"], files={"file": f})
    r.raise_for_status()
    print("2. App uploaded", path)

    try:
        record = finish_upload(started["media_id"])
    except Refused as e:
        print("3. Refused (tell the user):", e.code, "-", e.detail)
        return
    except TryLater:
        print("3. Temporary failure (retry later)")
        return
    print("3. Processed. Store this:")
    print(json.dumps(record, indent=2))


if __name__ == "__main__":
    main(sys.argv[1])
