# Functions

Functions run your code on demand, without a server you keep running. Your code starts when something calls it and stops when it is done, and you pay only while it runs.

## Overview

A function is a small piece of code (Node.js 22 or Python 3.13) that receives an **event**, does its job and returns a result. It starts when something calls it: an HTTPS address, the API, a schedule, a bucket, a queue or a webhook. Between calls nothing runs for you.

Each function runs in its own sandbox, on the private network of the project you choose. It can reach the project's databases and services next to it. A function can also be a **Web app**: an ordinary HTTP server (Express, Flask, FastAPI...) that starts on the first request and stops when idle.

- Open **Functions** in the Services menu. The page lists every function of the organization (or of the current project) with its runtime, whether its URL or schedule is on, and its invocations and errors over the last 24 hours.
- Type in **Find functions** to filter the list.
- Your plan sets how many functions you can have, and calls are billed per request and per GB-second.

## Use cases

- **Small API endpoint**: an API with little traffic. Turn on the **Function URL** and return JSON.
- **Webhook receiver**: accept events from GitHub, Stripe or a form provider with a **Webhook** trigger.
- **Image resize**: process each file after an upload with a **Bucket events** trigger on `.jpg` files.
- **Nightly cleanup or report**: set a **Schedule** such as `0 3 * * *`.
- **Queue worker**: process the messages of a queue in batches.
- **Small site that scales to zero**: choose **Web app** and pay nothing while nobody visits.
- **Glue between services**: call a database or an internal service over the private network (`http://api.shop.internal`).

## How to use it

### Create a function

Create a function in a project, starting from a working example.

1. On the **Functions** page, click **Create function** (developers).
2. Choose the **Project**. The function runs on that project's private network.
3. Enter a **Function name**, such as `resize-image`: 2 to 20 lowercase letters, digits and dashes.
4. Choose the **Kind**: **Function** (a handler called with each event) or **Web app** (see [Web apps that scale to zero](#web-apps-that-scale-to-zero)).
5. Choose the **Runtime**: **Node.js 22** or **Python 3.13**.
6. Click **Create function**. It starts with an example you can run from the **Test** tab.

```
PUT /api/v1/projects/PROJECT/functions/NAME
```

```
platformctl fn deploy shop/resize-image --file index.mjs
```

### Write the code

A handler receives two arguments, the **event** (the input, as JSON) and the **context** (facts about this call), and returns a result.

Node.js, in `index.mjs` (handler `index.handler`):

```
export const handler = async (event, context) => {
  console.log("got", JSON.stringify(event));
  return { message: "Hello from " + context.functionName };
};
```

With CommonJS, write `exports.handler = async (event, context) => { ... }` in `index.js`. The runtime looks for `.mjs`, then `.js`, then `.cjs`.

Python, in `main.py` (handler `main.handler`):

```
def handler(event, context):
    print("got", event)
    return {"message": "Hello from " + context.function_name}
```

The **Handler** setting (Configuration tab) is `file.function`: the file name without its extension, a dot, and the function's name. A file in a folder is written `src/app.main`. The handler in the page header must match your file and function names: when you rename one, change the handler too.

What your code can rely on:

- Everything it prints (`console.log`, `print`, errors) becomes the invocation's **log**, shown on the **Test** and **Monitor** tabs (up to 32 KB per call).
- It runs as an unprivileged user, in UTC (`TZ=UTC`), in its own sandbox. Its code is read-only; `/tmp` is writable.
- The variable `PLATFORM_FUNCTION` holds `project/function`.
- Return any value that can be turned into JSON. Throwing an error (or raising an exception) marks the call as failed, with the error's name and message.

### Edit in the browser or upload a zip

Change the code of a deployed function.

1. Open the **Code** tab, edit the handler's file and click **Deploy**. Saved code runs on the next invocation.
2. When the archive has several files, **All files** lists them. A file that is too large or not text cannot be edited here: upload a new archive instead.
3. Or, in **Upload a zip archive** (same tab), choose a `.zip` with your files and their dependencies (`node_modules`, or Python packages installed into the folder with `pip install -t . requests`), then click **Upload and deploy**. It replaces all the code.

The handler's file must be at the top level of the archive, or match the handler's path.

```
platformctl fn deploy shop/resize-image --zip build.zip
platformctl fn deploy shop/resize-image --file main.py --runtime python3.13 --handler main.handler
```

### Test events

Run the deployed code with an event of your choice before anything real calls it.

1. Open the **Test** tab and type a JSON event in **Test event**, such as `{"key": "photos/a.jpg"}`.
2. Click **Test**.
3. Read the **Result**: the status, the duration, whether it was a cold start, the request id, the returned value, the error type and message if it failed, and the **Log**.

### Configure a function

Change the settings of a function on the **Configuration** tab.

1. Under **Settings**, change the values you need (see [Settings](#settings)).
2. Click **Save configuration**.

Environment variables are written `NAME=value`, one per line; lines starting with `#` are ignored. Names use uppercase letters, digits and underscores.

- The project's shared variables are added too (see [Project variables](/docs/variables)).
- Set a value to `${secret:NAME}` to use a secret of the secrets manager (see [Project variables](/docs/variables)). The function picks up a new secret version at its next start.
- Use the connection URLs of the project's databases as variables: the function reaches them over the private network, as well as services by their private names (`http://api.shop.internal`).

### Turn on the function URL

Give the function its own HTTPS address, for browsers, apps and other services.

1. On the **Configuration** tab, tick **Function URL** and click **Save configuration**. The address is shown under **Function URL**: `https://<function>-<project>-<organization>.fn.{{apps}}/`.
2. Choose **Who may call the URL**: **anyone (public)** needs no authentication; **callers with the token** requires a token.
3. With **callers with the token**, click **Create a token** under **Function URL** and copy the token: it is shown once.
4. Callers send the token in the `X-Function-Token` header; other calls get `403 Forbidden`.
5. **Replace the token** makes a new one and stops the old one at once.

See [Function URL requests and responses](#function-url-requests-and-responses) for what your handler receives and returns.

### Run on a schedule

Run the function at fixed times, for reports, cleanups or syncs.

1. On the **Configuration** tab, enter a cron expression in UTC in **Schedule**, such as `0 3 * * *` (every day at 03:00) or `*/15 * * * *` (every 15 minutes). At most once a minute.
2. Tick **run on the schedule** and click **Save configuration**. The next run time appears next to it.

### Run on bucket events

Run the function each time a file is uploaded to or deleted from a bucket, for thumbnails, virus scans or indexing.

1. Open the **Triggers** tab. Under **Bucket events**, choose the **Bucket** (create one in [Object storage](/docs/object-storage) first).
2. Choose **On**: **uploads**, **deletes**, or **uploads and deletes**.
3. Optionally narrow it with a **Key prefix** (`images/`) and a **Key suffix** (`.jpg`).
4. Click **Add bucket trigger**. It applies within about 20 seconds. **remove** deletes it.

A function that writes into the bucket it listens to should use a prefix or suffix that its own files do not match, or it calls itself forever.

```
POST /api/v1/projects/PROJECT/functions/NAME/triggers
{"bucket": "uploads", "events": "created", "prefix": "images/", "suffix": ".jpg"}
```

### Process a queue

Let the function process the messages of a queue in batches. Set it up on the queue's **Settings** tab (see [Queues](/docs/queues)). Queue batches are the only background runs that are retried.

### Receive webhooks

Get a secret address for services that send webhooks.

1. On the **Triggers** tab, under **Webhook**, click **Create a webhook URL**.
2. Copy the URL (`{{console}}/hooks/fn/whk_...`): it is shown once. Anyone with it can start the function.
3. Give it to the other service.
4. **Replace the webhook URL** revokes the old one; **remove the webhook** turns it off.

```
POST /api/v1/projects/PROJECT/functions/NAME/webhook
```

The answer carries the new `url`. A sender over 120 calls a minute from one address, or a replaced or removed URL, gets `404`. Check the sender's signature header in your code before trusting the body.

### Web apps that scale to zero

Run an ordinary web application (Express, Fastify, Flask, FastAPI, a plain HTTP server) that starts on the first request and stops when idle. Use it for a small site or API without paying for an always-on service.

1. Create a function and choose **Web app**. It starts with a small HTTP server (`server.mjs` or `app.py`) and its start command (`node server.mjs`, `python3 -u app.py`).
2. Make your server listen on the port in the `PORT` variable (8080 unless you change **Web app port**, 1024 to 65535).
3. Change the **Web app start command** on the Configuration tab when needed, for example `node dist/index.js` or `python3 -m uvicorn main:app --host 0.0.0.0 --port 8080`. Include the dependencies in the zip.
4. Turn on the **Function URL**: each request (method, path, query, headers, cookies, body) is forwarded to your server, with `X-Forwarded-For` (the caller's address), `X-Forwarded-Proto: https` and `X-Platform-Request-Id`. Its answer (status, headers, cookies, body) is returned as is.

### Monitor invocations

See how a function behaves over the last 24 hours.

1. Open the **Monitor** tab. It shows the number of **Invocations**, **Errors**, the **Average duration** and the number of **Cold starts**.
2. Under **Recent invocations**, each call shows when it ran, its trigger (`url`, `api`, `test`, `schedule`, `bucket`, `queue`, `webhook`), its status (with a **cold** badge for cold starts), its duration, its error, and its **log**.

```
GET /api/v1/projects/PROJECT/functions/NAME/invocations
```

```
platformctl fn logs shop/resize-image
```

### Run a function from the API or CLI

Call a function yourself, from a script or another service.

```
POST /api/v1/projects/PROJECT/functions/NAME/invoke
```

```
platformctl fn invoke shop/resize-image --event '{"key": "a.jpg"}'
```

`--event -` reads the event from standard input, `--async` queues it. With `fn deploy --file`, the file becomes the handler's file; without `--handler` the handler is the file name plus `.handler`, so for Python always pass `--handler`. See [Invoke](#invoke) for headers and answers.

### Delete functions

1. At the bottom of the **Configuration** tab, click **Delete function** and type the function's name to confirm. Its URL, schedule, triggers and history go with it.
2. Or, on the **Functions** page, select several functions, choose **Turn off the URL** or **Delete** (type `delete` to confirm), then click **Apply**.

```
DELETE /api/v1/projects/PROJECT/functions/NAME
```

```
platformctl fn delete shop/resize-image
```

## Technical reference

### Limits and plans

| Item | Limit/Default | Notes |
| --- | --- | --- |
| Number of functions | set by your plan | default 20; trial 3; pro 200 |
| Function name | 2 to 20 characters | lowercase letters, digits, dashes |
| Runtimes | `nodejs22`, `python3.13` | |
| Memory | 128, 256, 512, 1024, 1536, 2048 MB | CPU grows with memory |
| Timeout | 1 to 300 seconds | longer calls are stopped and marked timed out |
| Concurrency | 1 to 50 | |
| Zip archive | 10 MB zipped, 50 MB unzipped, 2000 files | |
| Event and response | 6 MB each | |
| Log per call | 32 KB | |
| Environment variables | 4 KB in total | names starting with `PLATFORM_` and a few runtime names are reserved |
| Warm time | 10 minutes after the last call | |
| History | 7 days | invocations, logs and errors |
| Webhook body | 1 MB | at most 120 calls a minute from one address |
| Web app port | 1024 to 65535 | default 8080 |

### Settings

| Setting | Values | Effect |
| --- | --- | --- |
| **Memory** | 128 to 2048 MB | A slow function often gets faster with more. |
| **Timeout** | 1 to 300 s | A call running longer is stopped. |
| **Concurrency** | 1 to 50 | Invocations running at the same time. |
| **Handler** | `file.function` | Unused by a web app. |
| **Web app start command** and **Web app port** | text, 1024 to 65535 | Only for web apps. |
| **Function URL** and **Who may call the URL** | on/off; public or token | Turns the HTTPS endpoint on and chooses who may call it. |
| **Schedule** and **run on the schedule** | cron in UTC | The next run is shown once it is on. |
| **Environment variables** | `NAME=value` per line | See [Configure a function](#configure-a-function). |

### Handler context

| Node.js | Python | Meaning |
| --- | --- | --- |
| `context.functionName` | `context.function_name` | the function's name |
| `context.memoryLimitInMB` | `context.memory_limit_in_mb` | its memory setting |
| `context.getRemainingTimeInMillis()` | `context.get_remaining_time_in_millis()` | time left before the timeout |

### Function URL requests and responses

The request your handler receives as `event`:

| Field | Content |
| --- | --- |
| `rawPath` | the path, such as `/orders/42` |
| `rawQueryString` | the query string without `?`, such as `page=2&sort=asc` |
| `queryStringParameters` | the query as an object (repeated keys joined with commas); absent when there is none |
| `headers` | the request headers, names in lowercase, repeated headers joined with commas (the token header is removed) |
| `cookies` | a list of `name=value` strings; absent when there is none |
| `requestContext.http` | `method`, `path`, `protocol`, `sourceIp` and `userAgent` |
| `requestContext.requestId`, `requestContext.timeEpoch` | a request id and the time in milliseconds |
| `body` | the body as text, or base64 for binary bodies |
| `isBase64Encoded` | `true` when `body` is base64 |
| `version`, `routeKey` | always `"2.0"` and `"$default"` |

The response is built from what the handler returns:

- An object with a `statusCode` is the HTTP response: `statusCode`, `headers` (an object), `cookies` (a list of `Set-Cookie` values), `body` (a string) and `isBase64Encoded` (`true` when `body` is base64, for images or files). The content type is `application/json` unless you set one.
- A string is sent as the body, with status 200.
- Any other value (an object without `statusCode`, a list, a number) is sent as JSON with status 200.

Example, Node.js:

```
export const handler = async (event) => {
  const name = event.queryStringParameters?.name ?? "world";
  return { statusCode: 200, headers: { "content-type": "text/plain" }, body: `Hello ${name}` };
};
```

When things go wrong, the caller gets:

| Status | Meaning |
| --- | --- |
| `502` | the handler failed |
| `504` | it timed out |
| `429` | the function is throttled |
| `413` | body over 6 MB |
| `403` | missing or wrong token |

Every answer carries `X-Platform-Request-Id`, which you find again on the **Monitor** tab.

### Trigger events

Besides its URL and the API, a function can be started by a schedule, a bucket, a queue or a webhook. All of these run it in the background: nobody waits for the result, which you find on the **Monitor** tab. Failed background runs are not retried, except queue batches.

Schedule event:

| Field | Content |
| --- | --- |
| `detail-type` | `"Scheduled Event"` |
| `source` | `"platform.scheduler"` |
| `time` | the scheduled time, ISO 8601 |
| `id` | an id |
| `resources`, `detail` | empty |

Bucket event (a `Records` list with one record):

| Field | Content |
| --- | --- |
| `eventName` | `ObjectCreated:Put`, `ObjectCreated:CompleteMultipartUpload`, `ObjectCreated:Copy` or `ObjectRemoved:Delete` |
| `eventTime` | when it happened (ISO 8601, UTC) |
| `eventSource`, `eventVersion` | `"platform:s3"` and `"2.1"` |
| `s3.bucket.name` | the bucket's S3 name, such as `acme-uploads` |
| `s3.object.key` | the object's key |
| `s3.object.size` | its size in bytes (uploads only) |

Read the key with `event.Records[0].s3.object.key`. Batch deletes (`DeleteObjects`) are not reported. If the function falls far behind (20 times its concurrency waiting), new events are dropped rather than piling up.

Webhook event: each `POST` to the webhook URL answers `202` at once with an invocation id and runs the function in the background with the request as the event, in the function URL format above (`rawPath` is always `/`; the `authorization` and `cookie` headers are removed). Bodies up to 1 MB; at most 120 calls a minute from one address.

### API

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/projects/PROJECT/functions` | List functions |
| PUT | `/api/v1/projects/PROJECT/functions/NAME` | Create or update |
| DELETE | `/api/v1/projects/PROJECT/functions/NAME` | Delete |
| POST | `/api/v1/projects/PROJECT/functions/NAME/invoke` | Run it |
| GET | `/api/v1/projects/PROJECT/functions/NAME/invocations` | The last 50 invocations |
| POST | `/api/v1/projects/PROJECT/functions/NAME/triggers` | Add a bucket trigger |
| POST | `/api/v1/projects/PROJECT/functions/NAME/webhook` | Create or replace the webhook URL |

Listing and invocations need the viewer role; creating, deleting and invoking need the developer role. Adding a trigger and creating the webhook URL also need the developer role. A new function answers `201`, a deletion `204`.

#### Create or update

`PUT` creates the function if it does not exist, or updates it. Fields you leave out keep their value.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `runtime` | string | no | `nodejs22` (default) or `python3.13`; cannot change once the function exists |
| `handler` | string | no | `file.function` |
| `code` | string | no | source of the handler file |
| `filename` | string | no | file that `code` is written to (default: the handler's file) |
| `zip_base64` | string | no | a whole archive, base64 encoded |
| `memory_mb` | number | no | 128 to 2048 (default 128 for a new function) |
| `timeout_seconds` | number | no | 1 to 300 (default 10) |
| `concurrency` | number | no | 1 to 50 (default 5) |
| `env` | object | no | environment variables |
| `url_enabled` | boolean | no | turn the function URL on or off |
| `url_auth` | string | no | `none` or `token` |
| `schedule` | string | no | cron expression in UTC |
| `schedule_enabled` | boolean | no | run on the schedule |
| `web_command` | string | no | web app start command |
| `web_port` | number | no | web app port |

#### Invoke

The body is the event as JSON (empty means `{}`). The answer is the handler's result, with `X-Platform-Invocation-Id` and `X-Platform-Duration-Ms`.

| Request header | Effect |
| --- | --- |
| `X-Log-Type: Tail` | adds the last 4 KB of the log, base64 encoded, in `X-Platform-Log-Result` |
| `X-Invocation-Type: Event` | queues the call without waiting: the answer is `202` with `{"invocation_id": "...", "status": "queued"}` |

The event must be valid JSON (otherwise `400`) and under 6 MB (`413`). A throttled call gets `429` with `Retry-After: 1`. If the function does not answer in time, the call gets `504`. A failed call still answers `200`, with the header `X-Platform-Function-Error: Unhandled` and the body `{"errorType": "...", "errorMessage": "..."}`. Always check that header before using the body.

#### Add a bucket trigger

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `bucket` | string | yes | the bucket name |
| `events` | string | no | `created`, `removed` or `all` (default `created`) |
| `prefix` | string | no | key prefix, such as `images/` (200 characters at most) |
| `suffix` | string | no | key suffix, such as `.jpg` (50 characters at most) |

### Command-line tool

| Command | Purpose |
| --- | --- |
| `platformctl fn list PROJECT` | List the functions of a project |
| `platformctl fn deploy PROJECT/FN --file F` or `--zip Z` | Create or update from a source file or a zip (`--runtime`, `--handler` when needed) |
| `platformctl fn invoke PROJECT/FN [--event JSON\|-] [--async]` | Run it and print the result |
| `platformctl fn logs PROJECT/FN` | Recent invocations with their status and logs |
| `platformctl fn delete PROJECT/FN` | Delete a function |

### Environment variables

| Variable | Value |
| --- | --- |
| `PLATFORM_FUNCTION` | `project/function` |
| `TZ` | `UTC` |
| `PORT` | web apps only: the port your server must listen on (8080 by default) |

### Errors

A failed call has an error type and a message, shown on the Test and Monitor tabs and returned by the API.

| Error type | Meaning |
| --- | --- |
| the error's own name (`Error`, `TypeError`, `ValueError`...) | your handler threw an error or raised an exception |
| `Runtime.ImportModuleError` | the handler's file was not found or could not be loaded |
| `Runtime.HandlerNotFound` | the file has no function with the handler's name |
| `Runtime.UserCodeSyntaxError` | the file has a syntax error |
| `Runtime.MarshalError` | the Python result could not be turned into JSON |
| `Runtime.ExitError` | the process ended without a result; exit code 137 usually means out of memory |
| `Sandbox.Timedout` | the call ran past the timeout |
| `Function.ResponseSizeTooLarge` | the result is larger than 6 MB |
| `Platform.StartFailed` | the sandbox (or the web app's server) could not start |
| `Platform.InvokeFailed`, `WebApp.RequestFailed` | the call could not reach the code, for example after a crash |

### Behind the scenes

- **Cold and warm starts:** the first call after a while starts a fresh sandbox, a **cold start** of about a second (the very first call of a runtime on a server also downloads it). The sandbox then stays warm for 10 minutes after the last call, and warm calls start in a few milliseconds. Changing the code or the settings starts a new sandbox on the next call.
- **No state between calls:** each call runs your handler as a new process inside the sandbox, so variables at the top of your file are set up again every time. Keep data in a database or a bucket.
- **Queueing and throttling:** when more calls arrive than the concurrency allows, they wait their turn. Once four times the concurrency are waiting or running, new calls are refused with HTTP 429. Raise **Concurrency** if this happens in normal traffic.
- **Web apps:** other events (API invoke, console test, schedule, queue, bucket) are sent to your server as `POST /events` with the event as JSON; its answer (JSON or text) is the result. Webhook calls arrive as ordinary `POST /` requests. A status of 400 or more counts as an error. The server starts on the first request (it has 60 seconds to listen) and stays warm for 10 minutes after the last one. It handles concurrent requests itself. A server that crashes is started again on the next request. A web app's server keeps its memory while it is warm.
- **Billing:** per million requests and per GB-second (memory times run time), at your plan's prices. See [Billing](/docs/billing).

## Troubleshooting

### The call returns 200 but my code failed

Calls made through the API answer `200` even when the handler failed. Check the `X-Platform-Function-Error` header: `Unhandled` means the body holds `errorType` and `errorMessage`.

### Runtime.HandlerNotFound or Runtime.ImportModuleError

The **Handler** setting does not match your file and function names. Set it to `file.function` (for example `index.handler`), and check that the file is at the top level of the zip.

### Runtime.ExitError with code 137

The function ran out of memory. Raise **Memory** on the Configuration tab.

### Calls are refused with 429

More calls arrive than the concurrency allows. Raise **Concurrency**.

### A web app fails with Platform.StartFailed

Your server did not listen on the `PORT` variable within 60 seconds. Check the start command, the port and that dependencies are in the zip.

### A bucket trigger calls the function in a loop

The function writes into the bucket it listens to. Use a key prefix or suffix that its own files do not match.
