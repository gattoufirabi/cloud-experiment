# Queues

A queue holds messages between the part of your system that creates work and the part that does it. Messages wait safely until a worker is ready, so a slow or crashed worker never loses work.

## Overview

A producer (your website, say) sends a message such as "order 1042 was paid". A consumer (a worker service or a function) receives it, does the work and deletes it. If the consumer is slow, busy or down, messages wait in the queue instead of being lost.

Open **Queues** in the Services menu. The page lists each queue with its **Available** messages (waiting), **In flight** messages (received, not yet deleted) and its **Consumer** (a function, or your code through the API). Type in **Find queues** to filter.

Delivery is *at least once*: a message can occasionally be processed twice, so make your processing safe to repeat (for example, check whether order 1042 was already handled). Messages are handed out roughly oldest first. Your plan sets the number of queues, and queue API calls are billed per million requests.

## Use cases

- **Background jobs**: your web service sends "send the invoice" messages and a worker service handles them, so pages stay fast.
- **Absorb traffic spikes**: producers send as fast as they like, and consumers drain the queue at their own pace.
- **Serverless processing**: set a function as **Function trigger** and the platform calls it with batches of messages.
- **Retry with a safety net**: set a **Dead-letter queue** and **Max receives** so a poison message moves aside instead of blocking the rest.
- **Delayed work**: send with `delay_seconds` (up to 15 minutes) to run something a little later.
- **Least-privilege access**: connect a service with a scoped token that can only send, receive and delete on one queue.
- **Testing a consumer**: send a message by hand from the **Messages** tab.

## How to use it

### Create a queue

1. On the **Queues** page, enter a name such as `orders` (3 to 40 lowercase letters, digits and dashes).
2. Click **Create queue** (developers).
3. Adjust the defaults on the queue's **Settings** tab if you need to.

```
POST /api/v1/queues
{"name": "orders"}
```

```
platformctl queue create orders
```

### Send, receive and delete messages

Producers send, consumers receive, then delete once the work is done. All calls take an API token (or a queue token, see below) in `Authorization: Bearer`.

1. **Send** a message: `POST /api/v1/queues/QUEUE/messages` with `{"body": "…"}`.
2. **Receive** with long polling: `POST /api/v1/queues/QUEUE/receive?max=10&wait=20`.
3. **Delete** it with its receipt once processed.

```
platformctl queue send orders '{"order": 1042}'
platformctl queue receive orders --max 10 --wait 20
platformctl queue delete orders RECEIPT
```

A worker loop, in short: receive with `wait=20`; for each message, do the work, then delete it; repeat. Use `-` instead of the body to read it from standard input.

### Use the Messages tab

- **Send a message**: type a body and click **Send**, handy for testing a consumer.
- **Oldest messages**: shows the first waiting messages with when they were sent and how many times they were received. Viewing does not receive them.
- **purge**: deletes every message after you confirm.

### Change queue settings

1. Open the queue's **Settings** tab.
2. Change the values (see [Settings](#settings)).
3. Click **Save settings**.

To use a dead-letter queue, create it first (for example `orders-failed`), then choose it here. Without one, a message that always fails ("poison message") keeps coming back until its retention ends. With one, it moves aside after **Max receives**, so the other messages keep flowing, and you can look at it later on the dead-letter queue's **Messages** tab.

### Connect a service with a scoped token

Instead of giving a service a full API token, give it a token that only works for one queue.

1. On the queue's **Connect** tab, under **Connect to a service**, choose the service.
2. Click **Connect**. The service gets two secrets: `QUEUE_URL` (the queue's API address) and `QUEUE_TOKEN`.
3. Redeploy the service.

The token can only send, receive and delete this queue's messages: it cannot purge, change settings or reach anything else. The **From your code** box on the same tab shows the three `curl` calls ready to copy.

```
POST /api/v1/queues/QUEUE/attach
{"service": "project/service"}
```

### Process messages with a function

1. Create a function (see [Functions](/docs/functions)).
2. On the queue's **Settings** tab, choose it as **Function trigger**.
3. Set the **Batch size** and tick **trigger on**.
4. Click **Save settings**.

The platform then receives messages for you and calls the function with batches, as many batches at once as the function's concurrency allows. Untick **trigger on** to pause the function without losing messages. See [the event format](#function-event-format) and [what happens after the function runs](#what-happens-after-the-function-runs).

```
export const handler = async (event) => {
  const failed = [];
  for (const r of event.Records) {
    try { await handleOrder(JSON.parse(r.body)); }
    catch (e) { failed.push({ itemIdentifier: r.messageId }); }
  }
  return { batchItemFailures: failed };
};
```

Keep the function's timeout shorter than the queue's visibility timeout, or a batch can be handed out again while it is still running.

### Purge, list and delete queues

1. To delete every message, click **purge** on the **Messages** tab, or use the API or CLI below.
2. To delete a queue (admins), open its **Settings** tab, type the queue's name to confirm and click the delete button. Its messages are deleted.

```
platformctl queue purge orders
platformctl queue list
```

## Technical reference

### How delivery works

1. A producer **sends** a message. It becomes available at once, or after a delay you choose.
2. A consumer **receives** it. The message is not removed: it is hidden for the **visibility timeout** and comes with a **receipt**.
3. When the work is done, the consumer **deletes** the message with its receipt.
4. If the consumer never deletes it (it crashed, or the work failed), the message becomes available again after the visibility timeout and someone receives it again.

### Settings

| Setting | Values | Default | Effect |
| --- | --- | --- | --- |
| **Visibility timeout** | 1 to 43,200 seconds (12 hours) | 30 seconds | How long a received message stays hidden. Set it longer than your slowest processing, or messages come back while still being worked on. |
| **Retention** | 1 to 336 hours (14 days) | 4 days | How long a message is kept if nobody deletes it. |
| **Max receives** | 1 to 1000 | 5 | After that many receives without a delete, the message moves to the dead-letter queue. |
| **Dead-letter queue** | Another queue of the organization | none | Collects messages that failed too often. |
| **Function trigger** | A function | none | Processes the messages. |
| **Batch size** | 1 to 10 | 10 | Messages per function call. |
| **trigger on** | on or off | on | Untick to pause the function without losing messages. |

### Limits and plans

| Item | Limit | Notes |
| --- | --- | --- |
| Queue name | 3 to 40 lowercase letters, digits, dashes | Starts and ends with a letter or digit. |
| Message body | 1 byte to 256 KB | Send JSON as a string. |
| Send delay | 0 to 900 seconds | Up to 15 minutes. |
| Messages per send | up to 10 | With `{"messages": [...]}`. |
| Messages per receive | 1 to 10 | 1 by default. |
| Long-poll wait | 0 to 20 seconds | |
| Number of queues | Set by your plan | |

### API

| Method | Path | Role | Purpose |
| --- | --- | --- | --- |
| GET | `/api/v1/queues` | viewer | List queues (`name`, the settings, `function`, `available`, `in_flight`, `created_at`, `url`). |
| POST | `/api/v1/queues` | developer | Create a queue (`201`, answers with the queue). |
| DELETE | `/api/v1/queues/QUEUE` | admin | Delete a queue (`204`). |
| POST | `/api/v1/queues/QUEUE/messages` | developer | Send one message or up to 10. |
| POST | `/api/v1/queues/QUEUE/receive` | developer | Receive messages. |
| DELETE | `/api/v1/queues/QUEUE/messages/RECEIPT` | developer | Delete a message (`204`). |
| POST | `/api/v1/queues/QUEUE/purge` | developer | Delete every message (`204`). |
| POST | `/api/v1/queues/QUEUE/attach` | developer | Connect a service with a scoped token. |

A queue token (the one **Connect** creates) only works on the send, receive and delete message routes of its own queue: any other call answers `403`, and the messages of another queue answer `404`. The Role column is the role the API token needs.

Create a queue:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | The queue name, 3 to 40 lowercase letters, digits and dashes. A name already used returns `409`. |
| `visibility_timeout_seconds` | number | no | Visibility timeout, 1 to 43200 (30 by default). |
| `retention_seconds` | number | no | Retention, 60 to 1209600 (345600, 4 days, by default). |
| `max_receives` | number | no | Receives before the dead-letter queue, 1 to 1000 (5 by default). |
| `dead_letter_queue` | string | no | Name of another existing queue of the organization. |

Past your plan's number of queues the call returns `422` ("your plan allows N queues").

Send:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `body` | string | yes | Text of 1 byte to 256 KB. |
| `attributes` | object | no | String attributes, such as `{"source": "shop"}`. |
| `delay_seconds` | number | no | 0 to 900. |
| `messages` | array | no | Up to 10 items of the same shape, instead of a single `body`. |

The answer is `201` with `{"message_ids": […]}`.

Receive query parameters:

| Parameter | Values | Description |
| --- | --- | --- |
| `max` | 1 to 10 (default 1) | Messages to return. |
| `wait` | 0 to 20 seconds | When the queue is empty, wait this long for a message (long polling). |
| `visibility` | 0 to 43200 seconds | Overrides the visibility timeout for this receive. |

The answer is `{"messages": [...]}`. Each message has `id`, `receipt`, `body`, `attributes`, `receive_count`, `sent_at` and `first_received_at`. A receipt works only until the message is received again: deleting with an old receipt answers `404`.

Attach:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `service` | string | yes | `project/service`. |

### Command-line tool

| Command | Purpose |
| --- | --- |
| `platformctl queue create orders` | Create a queue. |
| `platformctl queue list` | List queues. |
| `platformctl queue send orders '{"order": 1042}'` | Send a message (`-` reads the body from standard input). |
| `platformctl queue receive orders --max 10 --wait 20` | Receive messages. |
| `platformctl queue delete orders RECEIPT` | Delete a message. |
| `platformctl queue purge orders` | Delete every message. |

### Environment variables

Set as secrets when you click **Connect** on the queue's **Connect** tab.

| Variable | Value |
| --- | --- |
| `QUEUE_URL` | The queue's API address. |
| `QUEUE_TOKEN` | A token limited to this queue (send, receive, delete). |

### Function event format

Each call's event is `{"Records": [...]}`, one record per message:

| Field | Content |
| --- | --- |
| `messageId` | The message's id. |
| `receiptHandle` | Its receipt. |
| `body` | The body, as text (parse JSON yourself). |
| `attributes.ApproximateReceiveCount` | How many times it has been received, including this time. |
| `attributes.SentTimestamp` | When it was sent, in milliseconds since 1970. |
| `attributes.ApproximateFirstReceiveTimestamp` | When it was first received, in milliseconds. |
| `messageAttributes` | Your attributes, each as `{"stringValue": "…", "dataType": "String"}`. |
| `eventSourceARN` | `plat:queue:<organization>:<queue>` |
| `eventSource` | `platform:queue` |
| `queue` | The queue's name. |

Records also carry `md5OfBody`, always empty, which you can ignore.

### What happens after the function runs

- The function **succeeds**: all the batch's messages are deleted.
- It succeeds but returns `{"batchItemFailures": [{"itemIdentifier": "<messageId>"}]}`: only the listed messages stay and come back after the visibility timeout. The others are deleted.
- It **fails or times out**: the whole batch comes back after the visibility timeout, and moves to the dead-letter queue after **Max receives**.

### Billing

Each API call to a queue (a send, even with 10 messages, a receive, a delete) counts as one request. Queue requests are billed per million.
