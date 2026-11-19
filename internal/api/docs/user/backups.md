# Backups and restore

Go back to an earlier copy of your service's data after a mistake, a bad update or a broken file. The persistent folder of each service is backed up every night.

## Overview

The persistent folder of each service is backed up every night. Backups are encrypted with a key unique to your organization. Services without a persistent folder hold no data to back up, and their **Backups** tab does not appear.

You can also take a snapshot on demand, change the hour of the nightly backup, or pause it. Restoring replaces the folder with a chosen snapshot, after a safety snapshot of the current data.

Managed databases have their own backups: see [Managed databases](/docs/databases).

## Use cases

- **Undo a mistake**: restore the snapshot from before the data was deleted or damaged.
- **Safe risky changes**: click **Back up now** before a migration or a big update.
- **Move the backup window**: set the nightly hour outside your busy period.
- **Pause backups**: untick **nightly at** while you test something, then tick it again.
- **Roll back a restore**: restore the snapshot marked **before restore**.
- **Consistent data**: dump your own database into the persistent folder with a [scheduled job](/docs/scheduled-jobs) so backups hold a usable copy.

## How to use it

### Schedule backups

1. Open the service and its **Backups** tab.
2. Choose an hour (UTC, 03:00 by default) next to **nightly at**.
3. Click **Save schedule** (developers and above).

To pause nightly backups, untick **nightly at** and click **Save schedule**. Tick it again to resume. The **Overview** tab shows "paused" while they are off.

### Back up now

Take a snapshot right now, for example before a risky change.

1. Open the service's **Backups** tab.
2. Click **Back up now** (developers and above).

Only one backup or restore runs at a time for a service.

### Read snapshots and jobs

- **Snapshots** lists every snapshot with its ID, when it was taken (UTC), its size and file count, and a badge: **nightly** or **before restore**.
- **Recent jobs** lists the latest backups and restores with their status and any error message.

### Restore a snapshot

Admins and owners can restore.

1. In **Snapshots**, find the one to go back to.
2. Type the service ID in the box next to it and click **Restore**, then confirm.
3. The platform takes a **safety snapshot** of the current data, stops the application, replaces the folder with the snapshot, and starts the application again.
4. If the restore was a mistake, restore the snapshot marked **before restore**.

### Keep database data consistent

Backups are taken while the application runs. If your service runs its own database, dump it to the persistent folder on a schedule (for example with a [scheduled job](/docs/scheduled-jobs)) so the backup holds a consistent copy.

## Technical reference

### Limits and defaults

| Item | Value | Notes |
| --- | --- | --- |
| Nightly hour | 0 to 23 (UTC) | 03:00 by default; an invalid hour falls back to 03. |
| Retention | 7 daily, 4 weekly, 6 monthly snapshots | The default policy, which your platform operator can change. Safety snapshots taken before a restore are always kept. |
| Concurrency | One backup or restore at a time per service | A second request is refused while one runs. |
| Snapshots shown | The 60 most recent | |
| Recent jobs shown | The 6 most recent | |
| Encryption | Key unique to your organization | |

### Settings

| Setting | Values | Default | Effect |
| --- | --- | --- | --- |
| **nightly at** (on or off) | ticked or not | on | Turns the nightly backup on or off. |
| Hour | 0 to 23 UTC | 3 | Time of the nightly backup. |

### Roles

| Action | Role |
| --- | --- |
| See the **Backups** tab | viewer and above |
| **Save schedule**, **Back up now** | developer |
| **Restore** | admin |

### Alerts

| Alert | When |
| --- | --- |
| `backup_failed` | A backup or restore job failed. |

### Behind the scenes

- A restore takes a safety snapshot of the current data, stops the application, replaces the folder with the snapshot, and starts the application again.
- The restore needs the exact service ID typed as confirmation.
- A service without a persistent folder cannot be backed up: the console says so.
- Backup, restore and schedule changes are recorded in the audit log.

## Troubleshooting

### The Backups tab is missing

The service has no persistent folder, so there is nothing to back up.

### A backup or restore is already running

Only one runs at a time for a service. Wait for it to finish in **Recent jobs**, then try again.

### Type the service ID exactly to confirm the restore

The confirmation box must contain the service ID exactly as shown.

### A backup failed

Open **Recent jobs** for the error message. A `backup_failed` alert is sent when a job fails.
