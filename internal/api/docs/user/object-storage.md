# Object storage

Object storage keeps files ("objects") in buckets: user uploads, images, documents, backups, exports, static websites. It speaks the S3 protocol, so any S3 tool or library works with it.

## Overview

A bucket is a container for objects, private by default. Because the storage speaks the S3 protocol, you can use the MinIO client and SDKs, rclone, s3cmd, Cyberduck, restic and most backup tools.

Open **Storage** in the Services menu. The top of the page shows how many buckets you have and how much of your plan's storage you gave to them. The page has three tabs: **Buckets**, **Access keys** and **Use from code**.

- **Buckets** hold your objects. Each has a quota, the most it may hold. The quotas of all your buckets together stay within your plan's storage.
- **Access keys** are the user name and password an S3 tool uses. Each key reaches only the buckets you choose.
- **Use from code** shows the connection settings and examples. To give a service of your organization its own key and the variables to use it, open a bucket and use its **Connect** tab, so you never copy keys by hand.

A bucket's S3 name is `<organization>-<name>`, for example `acme-uploads`: use that name in code and tools.

Plans: Trial includes 2 buckets and 1 GB, Starter 10 buckets and 10 GB, Pro 100 buckets and 100 GB. Stored data is billed per GB-month.

## Use cases

- **User uploads**: your service creates a presigned `PUT` URL and the browser sends the file straight to the bucket (see [Upload from the browser](#upload-from-the-browser)).
- **Private documents**: share an invoice through a presigned `GET` link that expires, for example with `rclone link --expire 1h`.
- **Static website**: tick **Public website** to serve a single-page app or generated documentation over HTTPS.
- **Backups and exports**: point rclone, restic or your backup tool at a bucket with a key limited to that bucket.
- **Image or file processing**: run a function each time an object is uploaded, to create thumbnails or scan files.
- **Shared assets for a service**: use **Connect** so your service gets `S3_*` variables for one bucket.
- **Keep a team tool away from production data**: give it a key with **read** access on one bucket only.

## How to use it

### Create a bucket

1. On the **Buckets** tab, under **Create a bucket**, enter a name such as `uploads` (3 to 40 lowercase letters, digits and dashes, starting and ending with a letter or digit).
2. Enter a **Quota** in MB. The form shows how much of your plan's storage is still free.
3. Click **Create bucket** (developers).

The bucket list shows each bucket's objects, use against its quota (refreshed every few minutes), and whether it is **private** or a **public website**.

```
POST /api/v1/buckets
{"name": "uploads", "quota_mb": 1024}
```

```
platformctl bucket create uploads --quota 2048
```

### List and delete buckets

To see your buckets, use the **Buckets** tab, `GET /api/v1/buckets` or `platformctl bucket list`.

To delete buckets in bulk, select them, choose **Delete with their objects (admins)**, type `delete` and click **Apply**. To delete one bucket, see [Change quota and delete a bucket](#change-quota-and-delete-a-bucket).

```
DELETE /api/v1/buckets/NAME
```

```
platformctl bucket delete uploads
```

### Create an access key

1. On the **Access keys** tab, under **Create an access key**, enter a **Name** (such as `laptop backups`).
2. For each bucket, choose **no access**, **read** or **read and write**.
3. Click **Create access key**.
4. Copy the **secret access key** at once. It is shown only now, with the access key ID, endpoint and region.

The tab lists every key, the buckets it reaches and how, and which service it belongs to when it was made by **Connect**. Click **delete** to revoke a key: clients using it lose access within seconds.

```
POST /api/v1/storage-keys
{"name": "ci", "buckets": {"uploads": "write"}}
```

```
platformctl key create ci uploads=write
platformctl key list
platformctl key delete ID
```

A key sees only its buckets: listing all buckets shows those only, other organizations' buckets do not exist for it, and a key cannot create or delete buckets.

### Connect a service to a bucket

To use a bucket from a service of your organization, connect it instead of copying keys.

1. Open the bucket, then its **Connect** tab.
2. Under **Connect to a service**, choose the service and click **Connect**.
3. Redeploy the service.

The platform creates a key for that service only (read and write on this bucket) and stores the secrets listed in [Environment variables](#environment-variables). Use the `S3_*` names in your own code. The **Connect** tab also shows the bucket's connection details, **Keys with access** to it, and the website address when the bucket is a public website.

```
POST /api/v1/storage/buckets/NAME/attach
{"service": "project/service"}
```

### Use it from code

The **Use from code** tab shows the connection settings (see [Connection settings](#connection-settings)). The examples below read the variables that **Connect** creates.

**Command line (rclone)**

Install rclone (https://rclone.org), then create a remote once and use it:

```
rclone config create store s3 provider Other endpoint "$S3_ENDPOINT" region "$S3_REGION" access_key_id "$S3_ACCESS_KEY_ID" secret_access_key "$S3_SECRET_ACCESS_KEY" force_path_style true
rclone lsf store:acme-uploads/reports/
rclone copy ./report.pdf store:acme-uploads/reports/
rclone sync ./public store:acme-site
rclone link --expire 1h store:acme-uploads/reports/report.pdf
```

`rclone link` prints a presigned download link; `rclone sync` makes the bucket match the folder (it deletes objects that are not in the folder).

**Node.js** (`npm install minio`)

```
import * as Minio from "minio";

const u = new URL(process.env.S3_ENDPOINT);
const s3 = new Minio.Client({
  endPoint: u.hostname,
  port: u.port ? Number(u.port) : (u.protocol === "https:" ? 443 : 80),
  useSSL: u.protocol === "https:",
  accessKey: process.env.S3_ACCESS_KEY_ID,
  secretKey: process.env.S3_SECRET_ACCESS_KEY,
  region: process.env.S3_REGION,
  pathStyle: true,
});
const bucket = process.env.S3_BUCKET;

await s3.putObject(bucket, "avatars/42.png", buffer, buffer.length, { "Content-Type": "image/png" });
const stream = await s3.getObject(bucket, "avatars/42.png");
const url = await s3.presignedGetObject(bucket, "avatars/42.png", 3600); // valid 1 hour
```

**Python** (`pip install minio`)

```
import os
from datetime import timedelta
from urllib.parse import urlparse
from minio import Minio

u = urlparse(os.environ["S3_ENDPOINT"])
s3 = Minio(u.netloc, access_key=os.environ["S3_ACCESS_KEY_ID"], secret_key=os.environ["S3_SECRET_ACCESS_KEY"],
           region=os.environ["S3_REGION"], secure=u.scheme == "https")
bucket = os.environ["S3_BUCKET"]

s3.fput_object(bucket, "reports/report.pdf", "report.pdf", content_type="application/pdf")  # multipart for large files
s3.fget_object(bucket, "reports/report.pdf", "/tmp/report.pdf")
for o in s3.list_objects(bucket, prefix="reports/", recursive=True):
    print(o.object_name, o.size)
url = s3.presigned_get_object(bucket, "reports/report.pdf", expires=timedelta(hours=1))
```

**Go** (`go get github.com/minio/minio-go/v7`)

```
u, _ := url.Parse(os.Getenv("S3_ENDPOINT"))
client, err := minio.New(u.Host, &minio.Options{
    Creds:        credentials.NewStaticV4(os.Getenv("S3_ACCESS_KEY_ID"), os.Getenv("S3_SECRET_ACCESS_KEY"), ""),
    Secure:       u.Scheme == "https",
    Region:       os.Getenv("S3_REGION"),
    BucketLookup: minio.BucketLookupPath,
})
_, err = client.PutObject(ctx, os.Getenv("S3_BUCKET"), "notes/today.txt",
    strings.NewReader("hello"), 5, minio.PutObjectOptions{ContentType: "text/plain"})
```

Other languages: any S3 library works with the endpoint, region, key, path-style addressing and Signature V4.

### Share objects with presigned URLs

A presigned URL lets someone download (`GET`) or upload (`PUT`) one object for a limited time, without a key: a private invoice link in an email, a direct upload from a mobile app.

1. Your backend creates the URL with its key (`presignedGetObject`, `presignedPutObject`, `presigned_get_object`, or `rclone link`).
2. You give the URL to the person or the page.
3. The link stops working when it expires or when that key is deleted.

Presigned `POST` (an HTML form that uploads a file) works too. With **Browser access** set, web pages can use these links directly.

### Upload from the browser

A web page can send a file straight to the storage, without passing it through your service, which saves your service's bandwidth and memory.

1. Open the bucket's **Settings** tab. Under **Browser access (CORS)**, enter your site in **Allowed sites**, such as `https://shop.example.com` (one per line, no path; `*` allows every site).
2. Click **Save browser access**. Leave the field empty to turn it off.
3. Your service creates a presigned `PUT` URL for the object. It keeps the key; the page never sees it:

```
const url = await s3.presignedPutObject(process.env.S3_BUCKET, `uploads/${userId}/${name}`, 300); // 5 minutes
```

4. The page uploads with it:

```
await fetch(url, { method: "PUT", body: file });
```

The same setting lets pages read objects with `fetch` through presigned `GET` URLs (or from a public website bucket). Pages of other sites are refused. Browser access alone never makes objects public: a page still needs a presigned URL.

```
PUT /api/v1/buckets/NAME/cors
{"origins": ["https://shop.example.com"]}
```

Send `{"origins": []}` to turn it off.

### Host a static website

A bucket can serve a static website (HTML, CSS, JavaScript, images), for example a single-page app or documentation built by a generator.

1. Open the bucket's **Settings** tab, under **Quota and website**.
2. Tick **Public website**.
3. Set the **Index document** (`index.html` by default), served for a folder, and optionally an **Error document** (`404.html`).
4. Click **Save settings**.
5. Upload your site, for example `rclone sync ./dist store:acme-site`.

The site answers at `https://<organization>-<name>.web.{{apps}}/`, with HTTPS (shown on the **Connect** tab). Every object of the bucket becomes readable by anyone: keep private files in another bucket. Untick **Public website** to make the bucket private again.

### Browse objects in the console

The bucket's **Objects** tab is a simple file browser.

1. Open folders, and go **up** to return.
2. Click a file to download it (through a link valid for 5 minutes).
3. Click **delete** on a file and confirm.
4. Use **Upload a file** to add a file of up to 25 MB into the current folder.

It shows the first 200 entries of a folder. Use an S3 tool to list everything, and for larger files or whole folders.

### React to new files

A function can run each time an object is uploaded to or deleted from a bucket, filtered by key prefix and suffix: create thumbnails, scan files, update a search index. Set it up on the function's **Triggers** tab (see [Triggers](/docs/functions#run-on-bucket-events)).

### Change quota and delete a bucket

1. Open the bucket's **Settings** tab.
2. To change the quota, enter a new value within your plan's storage and click **Save settings**.
3. To delete the bucket (admins), type the bucket's name to confirm and click the delete button.

Deleting removes the bucket with all its objects and cannot be undone. Access keys keep working for your other buckets.

## Technical reference

### Limits and plans

| Item | Limit / default | Notes |
| --- | --- | --- |
| Bucket name | 3 to 40 lowercase letters, digits, dashes | Starts and ends with a letter or digit. S3 name is `<organization>-<name>`. |
| Access key name | 1 to 60 characters | |
| Browser access (CORS) | Up to 20 allowed sites | `*` allows every site. |
| Quota | Per bucket, in MB | Writes beyond it are refused with `QuotaExceeded`. All quotas together stay within your plan's storage. |
| Trial | 2 buckets, 1 GB | |
| Starter | 10 buckets, 10 GB | |
| Pro | 100 buckets, 100 GB | |
| Single request | Up to 5 GB | Larger files use multipart upload. |
| Batch delete | Up to 1000 objects per call | |
| Console upload | 25 MB per file | S3 tools have no such limit. |
| Console file list | First 200 entries of a folder | |
| Console download link | Valid 5 minutes | |
| Billing | Per GB-month | |

### Connection settings

For any tool (the **Use from code** tab shows them too):

- **Endpoint:** `{{s3}}`. Some libraries want the host name and the HTTPS switch separately: take the host from the URL (without `https://`) and turn TLS on.
- **Region:** `{{s3region}}`.
- **Path-style addressing:** the bucket goes in the path, `{{s3}}/acme-uploads/file.txt`, not in the host name. Turn it on in your tool (`--path on`, `pathStyle: true`, `force_path_style`).
- **Signature V4**, the default of current S3 tools. Presigned URLs made with the old signature are refused.

### Environment variables

Created in the service's secrets by **Connect**:

| Variable | Value |
| --- | --- |
| `S3_ENDPOINT` | `{{s3}}` (a full URL, with `https://`) |
| `S3_REGION` | `{{s3region}}` |
| `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | The service's own key. |
| `S3_BUCKET` | The bucket's S3 name, such as `acme-uploads`. |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `AWS_DEFAULT_REGION`, `AWS_ENDPOINT_URL`, `AWS_ENDPOINT_URL_S3` | The same values under the conventional variable names many S3 libraries read by themselves. |

### What you can do with objects

- **Upload, download, delete** objects of any size: up to 5 GB in one request, larger files with multipart upload (rclone and the SDKs' upload helpers switch to it by themselves). Batch delete removes up to 1000 objects in one call.
- **Folders** are key prefixes: `reports/2026/march.pdf` sits in the folder `reports/2026/`. List one level with a prefix and the `/` delimiter; `rclone sync` copies whole trees.
- **Content type and metadata:** set `Content-Type`, `Cache-Control`, `Content-Disposition` and your own metadata (user metadata in your S3 library) when you upload. They come back on download and `HEAD`.
- **Partial downloads** with the `Range` header (video seeking, resumable downloads), conditional requests, and **server-side copies** (`CopyObject`) without downloading.

### Supported S3 operations

| Works | Not available |
| --- | --- |
| PutObject, GetObject, HeadObject, DeleteObject, DeleteObjects | Bucket policies and ACLs (use keys with per-bucket access) |
| ListObjectsV2 / ListObjects (prefix, delimiter, pagination) | Object tagging |
| CopyObject, ranges, conditional requests | Versioning and object lock |
| Multipart upload (create, upload part, complete, abort, list) | Lifecycle rules; CORS through S3 (use **Browser access** in the bucket's settings) |
| Presigned GET, PUT and POST | Creating or deleting buckets through S3 (use the console, the API or the CLI) |
| ListBuckets (the key's buckets), checksums | Storage classes, replication, archive tiers |

Server-side encryption headers are accepted and ignored: data is stored on the platform's own servers.

### API

| Method | Path | Role | Purpose |
| --- | --- | --- | --- |
| GET | `/api/v1/buckets` | viewer | List buckets (`name`, `s3_name`, `quota_mb`, `objects`, `bytes`, `website_url`, `endpoint`, `region`, `created_at`). |
| POST | `/api/v1/buckets` | developer | Create a bucket (`201`, answers with the bucket). |
| PUT | `/api/v1/buckets/NAME/cors` | developer | Set Browser access (CORS). |
| DELETE | `/api/v1/buckets/NAME` | admin | Delete a bucket with its objects (`204`). |
| POST | `/api/v1/storage/buckets/NAME/attach` | developer | Connect a service to a bucket. |
| GET | `/api/v1/storage-keys` | viewer | List access keys (`id`, `name`, `access_key_id`, `service`, `buckets`, `created_at`). |
| POST | `/api/v1/storage-keys` | developer | Create an access key (`201`; the answer holds `secret_access_key`, shown only once). |
| DELETE | `/api/v1/storage-keys/ID` | developer | Revoke an access key, by its ID or its access key ID (`204`). |

The calls answer `404` when object storage is not enabled on the platform.

Create a bucket:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | Bucket name (see the limits above). A name already used returns `409`. |
| `quota_mb` | number | no | Quota in MB, from 1 up to the storage still free in your plan. When omitted: 1024 MB, or what is left if less. |

Create an access key:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | Key name, 1 to 60 characters. |
| `buckets` | object | yes | Bucket name to `read` or `write` (read and write), such as `{"uploads": "write"}`; at least one bucket. |

Set Browser access:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `origins` | array of strings | yes | Allowed sites such as `https://shop.example.com` (no path, at most 20), or `*`. `[]` turns it off. |

Connect a service:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `service` | string | yes | `project/service`. |

The call creates the service's key and its secrets (see [Environment variables](#environment-variables)), then answers `{"message": "…"}`.

### Command-line tool

| Command | Purpose |
| --- | --- |
| `platformctl bucket list` | List buckets. |
| `platformctl bucket create uploads --quota 2048` | Create a bucket with a quota in MB (1024 MB if `--quota` is left out). |
| `platformctl bucket delete uploads` | Delete a bucket. |
| `platformctl key create ci uploads=write` | Create an access key. |
| `platformctl key list` | List access keys. |
| `platformctl key delete ID` | Revoke an access key (by ID or access key ID). |

### Website address

A public website bucket answers at `https://<organization>-<name>.web.{{apps}}/`, with HTTPS.

## Troubleshooting

### Uploads fail with QuotaExceeded

The bucket has reached its quota. Raise it in the bucket's **Settings** tab, within your plan's storage.

### Presigned URLs are refused

Presigned URLs made with the old signature are refused. Use Signature V4, the default of current S3 tools, and path-style addressing.

### A web page cannot upload or read

Add your site to **Allowed sites** under **Browser access (CORS)**. Pages of other sites are refused, and the page still needs a presigned URL.
