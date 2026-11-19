# Billing and usage

See what your organization uses and what it costs: this month so far, the cost of each project, budgets that warn you before you overspend, your invoices and the details printed on them.

## Overview

The **Billing** page shows your usage and its price. Admins and owners open it from the product menu (**Billing** › **Usage and invoices**) or from **Usage of your plan** on the home page.

It has these parts:

- **This month so far**: the estimated amount up to now, one line per billed item, and usage by resource.
- **Cost by project**: this month's usage priced per project, with a forecast.
- **Budgets**: monthly limits that warn you when spending gets close.
- **Invoices**: one per calendar month.
- **Billing details**: the name, address and tax ID printed on your invoices.

### What is billed

- Your plan's monthly fee. The fee of your first month is counted from the day the organization was created.
- The CPU and memory **allocated** to each running service and database, per hour, at the size you chose (not the share the application actually used).
- Network traffic going out of your services, per GB.
- Builds from git, per build minute.
- Object storage, per GB-month of data stored in your buckets (measured every hour).
- Functions, per million requests and per GB-second (memory × run time of each invocation).
- Queues, per million requests.
- Email sent through the platform, per thousand emails.

Stopped or deleted resources cost nothing. The prices of your plan are listed under the current month's table.

### Disk limits

Each plan includes an amount of persistent storage, so one organization cannot fill the servers' disks. It counts service persistent folders and managed databases (with their backup dumps). Object storage buckets have their own quotas (see [Object storage](/docs/object-storage)).

## Use cases

- **Check this month's bill**: open **This month so far** for the estimate and each billed item.
- **Find the expensive project**: read **Cost by project** and compare **so far**, **Forecast for the month** and **Last month**.
- **Avoid surprises**: set a budget on the whole organization with thresholds at 50, 80 and 100 percent.
- **Cap one project**: save a budget for a single project and get warned when its forecast passes the limit.
- **Get an invoice for accounting**: open it and use **Print or save as PDF**.
- **Put your company name on invoices**: fill in **Billing details**.
- **Track costs from a script**: call the costs and budgets API with an admin token.
- **Stay under the disk limit**: watch **Disk** under **Usage of your plan** on the home page.

## How to use it

### Read this month's usage

Understand what you are paying for right now.

1. Open **Billing** › **Usage and invoices**.
2. In **This month so far**, read the estimated amount (with tax if your operator charges it) and one line per billed item, with quantity, unit price and amount. It updates every minute.
3. In **Usage by resource**, read each service, database and bucket with its CPU (vCPU-hours), memory (GB-hours), network out (GB) and storage (GB-month).

### See the cost by project

Find out which project spends what.

1. On the **Billing** page, open **Cost by project**.
2. Read the three columns: **so far**, **Forecast for the month** and **Last month**. Amounts are before tax.

Object storage, queues, email and the plan fee belong to the whole organization and appear as **Organization-wide**.

```
GET /api/v1/costs
GET /api/v1/costs?month=2026-10
```

### Set a budget

A budget warns you when spending gets close to a monthly limit you set, for the whole organization or for one project.

1. In **Budgets**, choose the scope: **Whole organization** or **Project …**.
2. Enter the monthly limit in your currency (for example `50`).
3. Enter the alert thresholds in % of the limit, separated by commas (`50, 80, 100` by default; up to 5 values, from 1 to 500).
4. Keep **warn on forecast** ticked to be warned as soon as the forecast for the month passes the limit.
5. Click **Save budget**.

Every hour the platform compares the month's spending with each budget. When a threshold is first crossed, or the forecast passes the limit, your [alert channels](/docs/alerts#notification-channels) are notified (event `budget`). A budget only warns: nothing is ever stopped.

```
POST /api/v1/budgets
{"project": "shop", "limit_cents": 5000, "thresholds": [50, 80, 100], "forecast_alert": true}
```

### Read, change or delete a budget

1. In the budgets table, read each budget's limit, what was spent (and the %), the forecast, and its thresholds. Amounts over the limit are shown in red.
2. To change a budget, save it again for the same scope. This replaces its limit and thresholds and starts its alerts over for the month.
3. Click **Delete** to remove a budget.

```
GET /api/v1/budgets
DELETE /api/v1/budgets/ID
```

### Open and pay an invoice

1. In **Invoices**, find the month and its status: **to pay**, **paid** or **cancelled**.
2. Click the invoice number to open it.
3. Click **Print or save as PDF** to print it or save it from your browser.
4. To pay, follow the payment instructions you received from your platform operator (see [the admin guide](/docs/admin)). The invoice shows **paid** once the operator has recorded your payment.

If the page says invoicing is not turned on for this installation, usage is still measured but no invoices are issued.

### Edit billing details

1. In **Billing details**, enter the **Company or person**, the **Address** and the **Tax / VAT ID** to print on your invoices. Leave the name empty to use the organization's name.
2. Click **Save billing details**.

Changes apply to the next invoices, not to ones already issued.

### Check your disk usage

1. Open the organization's home page.
2. Under **Usage of your plan**, read **Disk**: used and allowed, in MB.
3. To free space, delete old files from persistent folders, drop unused tables or databases, and delete services you no longer need.

## Technical reference

### Limits and plans

| Item | Limit or default | Notes |
| --- | --- | --- |
| Disk, Trial plan | 2 GB | Default plans; your operator may offer others. |
| Disk, Starter plan | 10 GB | |
| Disk, Pro plan | 50 GB | |
| Disk measurement | Every 15 minutes | Counts persistent folders and databases with their backup dumps. |
| Alert thresholds per budget | Up to 5 values, 1 to 500 % | `50, 80, 100` by default. |
| Budgets per scope | 1 | Saving again replaces it. |
| Budget check | Every hour | |
| Usage page refresh | Every minute | |
| Invoice period | One calendar month (UTC) | Issued on the 1st of the next month. |

### Disk limit behavior

| Usage | What happens |
| --- | --- |
| 90% and 100% of the limit | Every alert channel gets a "Disk limit" notification. |
| Above 110% | New deployments and new databases are refused ("Disk limit exceeded…") until you delete data or move to a larger plan. |
| Always | Running applications keep running, and rolling back is always allowed. |

### Invoice statuses

| Status | Meaning |
| --- | --- |
| to pay | Issued, payment not recorded yet. |
| paid | Your operator recorded your payment. |
| cancelled | The invoice was cancelled. |

### Billing details fields

| Field | Description |
| --- | --- |
| Company or person | Name printed on invoices; empty uses the organization's name. |
| Address | Postal address printed on invoices. |
| Tax / VAT ID | Tax identifier printed on invoices. |

### API

All these calls need an admin token (see [API tokens](/docs/api-and-cli#api-tokens)).

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/costs` | Cost report for the current month. Add `?month=YYYY-MM` for an earlier month (not in the future). |
| GET | `/api/v1/budgets` | List budgets. |
| POST | `/api/v1/budgets` | Create or replace a budget (answers 201). |
| DELETE | `/api/v1/budgets/{id}` | Delete a budget (answers 204). |

Body of `POST /api/v1/budgets`:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `project` | string | No | Project slug. Empty or missing means the whole organization; an unknown project answers `422`. |
| `limit_cents` | integer | Yes | Monthly limit in cents; must be above 0 (a missing or zero value answers `422`). |
| `thresholds` | array of integers | No | Alert thresholds in % of the limit, up to 5 values from 1 to 500; `[50, 80, 100]` when omitted. |
| `forecast_alert` | boolean | No | Warn when the forecast passes the limit; on when omitted. |

### Console endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/o/<org>/billing` | Billing page. |
| POST | `/o/<org>/billing/details` | Save billing details (fields `billing_name`, `billing_address`, `billing_tax_id`). |
| POST | `/o/<org>/billing/budgets` | Save a budget. |
| POST | `/o/<org>/billing/budgets/<id>/delete` | Delete a budget. |
| GET | `/o/<org>/invoices/<number>` | Open an invoice. |

## Troubleshooting

### Disk limit exceeded

New deployments and databases are refused above 110% of your disk limit. Delete data from persistent folders or databases, delete unused services, or ask for a bigger plan. Rolling back is still allowed.

### No invoices appear

The page says invoicing is not turned on for this installation. Usage is still measured; ask your platform operator.

### The forecast looks too high

The forecast assumes the rest of the month continues at the same pace. It drops as usage drops, and stopped or deleted resources cost nothing.
