# Artifact store (S3-compatible or filesystem)

All services that touch artifacts (`api`, `worker-agent`, `signer`, `previewd`)
pick their store from the environment (`internal/blob.FromEnv`). Objects are
content-addressed: key `<prefix>sha256/<hex>`, written once, never changed.

## Filesystem (default, single host)

```
FOUNDRY_BLOBS=/var/lib/foundry/blobs
```

## S3-compatible bucket (AWS S3, MinIO, Ceph RGW, …)

```
FOUNDRY_S3_BUCKET=foundry-artifacts      # selects the S3 backend
FOUNDRY_S3_ENDPOINT=https://minio.internal:9000   # empty = AWS (https://s3.<region>.amazonaws.com)
FOUNDRY_S3_REGION=us-east-1
FOUNDRY_S3_ACCESS_KEY=…  FOUNDRY_S3_SECRET_KEY=…  [FOUNDRY_S3_SESSION_TOKEN=…]
FOUNDRY_S3_PREFIX=prod/                  # optional
FOUNDRY_S3_PATH_STYLE=1                  # default when an endpoint is set; 0 = virtual-hosted
FOUNDRY_S3_PART_MIB=64                   # multipart part size (≥ 5)
FOUNDRY_S3_CONCURRENCY=4                 # parallel part uploads (≤ 32)
FOUNDRY_S3_PRESIGN=1                     # api only: redirect downloads to presigned URLs
```

Behaviour:

- **Upload** streams the input, hashing it while reading. Objects below one
  part are a single `PUT` straight to their final key. Larger ones use a
  multipart upload (64 MiB parts, 4 in flight; memory is bounded by
  `(concurrency+1) × part size`) to `tmp/upload-<random>`; after the digest
  (and the caller's expected digest) is verified, the object is copied to
  `sha256/<hex>` (ranged copy above 4 GiB) and the temporary object is deleted.
  A wrong or truncated upload therefore never occupies an immutable name, and
  failures abort the multipart upload.
- Transient errors (5xx, 429, `SlowDown`, network) are retried with backoff.
- **Download**: `GET /v1/blobs/{digest}?exp&sig` (HMAC-signed by the API) either
  streams via ranged GETs, or — with `FOUNDRY_S3_PRESIGN=1` — answers `302` to a
  5-minute presigned S3 URL, so artifacts never pass through the API.
  The S3 endpoint must then be reachable by clients.
- Anything that needs a local file (disk scan, deb validation, previews)
  fetches a verified temporary copy (`blob.Local`).
- Give the bucket a lifecycle rule that expires `tmp/` after one day (covers a
  crashed uploader) and enable versioning off: objects are immutable by key.
- Credentials need `s3:GetObject`, `PutObject`, `DeleteObject`,
  `AbortMultipartUpload`, `ListBucketMultipartUploads` on the bucket.
