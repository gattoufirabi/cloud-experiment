# Scheduled jobs

Run a command on a schedule, such as a nightly report, cleaning old records or sending reminder emails, without keeping an extra service running. Each run uses your service's image, variables and private network.

## Overview

A job runs a command on a schedule. It runs in a fresh container made from the service's current deployment: same image, environment variables, secrets, project variables and private network, so it reaches the project's databases. There is no extra service to keep running.

- Jobs belong to a service and are managed on its **Jobs** tab.
- Schedules use the classic cron format, in UTC.
- The command runs with `/bin/sh -c`, so the image needs a shell.
- Each run is recorded with its status, duration and the end of its output.
- Jobs use the service's CPU and memory, counted in your usage for the minutes they run.

## Use cases

- **Nightly report**: `0 3 * * *` with `python manage.py nightly_report`.
- **Clean old records**: run a cleanup command every night or every week.
- **Reminder emails**: `0 8 * * MON-FRI` with `python manage.py send_reminders`, using [email](/docs/email).
- **Warm a cache**: `*/15 * * * *` calling a command that preloads data.
- **Run a task by hand**: click **run now** to start a maintenance task at once, or `platformctl job run`.
- **Pause during maintenance**: **pause** stops the schedule without deleting the job.
- **Be told when it fails**: a failed or timed-out run sends a **Scheduled job failed** alert (see [Alerts](/docs/alerts)).

## How to use it

### Create or change a job

1. Open the service and its **Jobs** tab.
2. Under **Add or change a job**, enter a **Job ID** (`nightly-report`: 2 to 30 lowercase letters, digits and dashes; an existing ID updates that job), a **Schedule** (suggestions appear as you type), a **Command** such as `python manage.py send_reminders` (up to 2000 characters), and a **Time limit** in seconds.
3. Click **Save job**.

```
PUT /api/v1/projects/PROJECT/services/SERVICE/jobs/JOB
{"schedule": "0 3 * * *", "command": "…", "timeout_seconds": 600, "enabled": true}
```

### Run, pause, resume or delete a job

In the jobs table:

1. **run now** starts the job at once.
2. **pause** stops the schedule without deleting the job; **resume** starts it again.
3. **delete** removes the job and its run history.

```
POST /api/v1/projects/PROJECT/services/SERVICE/jobs/JOB/run
DELETE /api/v1/projects/PROJECT/services/SERVICE/jobs/JOB
```

```
platformctl job run shop/web JOB --wait
```

`--wait` prints the job's log and exit status.

### Read the runs

**Recent runs** lists each run with its status (running, succeeded, failed, timed out, skipped), whether it was started by hand, when, how long it took, and the **last lines** of its output with a link to the **full log**.

```
GET /api/v1/projects/PROJECT/services/SERVICE/jobs
GET /api/v1/projects/PROJECT/services/SERVICE/jobs/JOB/runs
GET /api/v1/projects/PROJECT/services/SERVICE/jobs/JOB/runs/ID
```

The last call returns the run with its log.

```
platformctl job list shop/web
platformctl job logs shop/web JOB
```

### List every job of the organization

**Compute** → **Scheduled jobs** lists every job of the organization (or of the filtered project) with its schedule, last run and next run. Click a job to open its service's **Jobs** tab.

## Technical reference

### Schedule format

Classic cron, in UTC: `minute hour day-of-month month day-of-week`.

| Schedule | Runs |
| --- | --- |
| `*/15 * * * *` | every 15 minutes |
| `0 3 * * *` | every day at 03:00 UTC |
| `0 8 * * MON-FRI` | weekdays at 08:00 UTC |
| `0 4 * * SUN` | every Sunday at 04:00 UTC |
| `0 2 1 * *` | the 1st of each month at 02:00 UTC |
| `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly` | shortcuts |

### Limits and defaults

| Item | Limit/Default | Notes |
| --- | --- | --- |
| Jobs per service | 20 | Previews have no jobs. |
| Job ID | 2 to 30 lowercase letters, digits and dashes | Starts with a letter or digit. |
| Command length | 2000 characters | Runs with `/bin/sh -c`. |
| Time limit | 10 seconds to 1 hour, 10 minutes by default | A run over it is stopped and marked **timed out**. |
| Runs kept | last 50 per job | With exit code and the end of the output (up to 64 KB of log per run). |
| Overlap | none | A run due while the previous one is still going is recorded as **skipped**. |
| Persistent folder | not mounted | The job does not get the service's persistent folder. |

### Run statuses

| Status | Meaning |
| --- | --- |
| running | The run is in progress. |
| succeeded | The command exited with code 0. |
| failed | The command failed. Sends a **Scheduled job failed** alert. |
| timed out | The run went over its time limit and was stopped. Sends the same alert. |
| skipped | The previous run was still going. |

### API

| Method | Path | Role | Purpose |
| --- | --- | --- | --- |
| GET | `/api/v1/projects/PROJECT/services/SERVICE/jobs` | viewer | List jobs (`id`, `schedule`, `command`, `timeout_seconds`, `enabled`, `next_run_at`, `run_requested`, `last_status`, `last_run_at`). |
| PUT | `/api/v1/projects/PROJECT/services/SERVICE/jobs/JOB` | developer | Create or update a job. |
| DELETE | `/api/v1/projects/PROJECT/services/SERVICE/jobs/JOB` | developer | Delete a job (`204`). |
| POST | `/api/v1/projects/PROJECT/services/SERVICE/jobs/JOB/run` | developer | Run a job now (`202`, `{"status": "requested"}`). |
| GET | `/api/v1/projects/PROJECT/services/SERVICE/jobs/JOB/runs` | viewer | List the 20 latest runs (`id`, `job`, `trigger`, `status`, `exit_code`, `started_at`, `finished_at`), without the log. |
| GET | `/api/v1/projects/PROJECT/services/SERVICE/jobs/JOB/runs/ID` | viewer | One run, with its log. |

Create or update (`PUT …/jobs/JOB`):

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `schedule` | string | yes | Cron schedule, in UTC (`minute hour day-of-month month day-of-week`; ranges, lists, steps, `JAN` or `MON` names and the shortcuts below). |
| `command` | string | yes | Command to run, up to 2000 characters. |
| `timeout_seconds` | number | no | Time limit, 10 to 3600 seconds (600 by default). |
| `enabled` | bool | no | `false` pauses the job; the job is enabled when the field is omitted. |

The call replaces the job's settings (a missing `timeout_seconds` becomes 600, a missing `enabled` becomes `true`) and answers `{"id", "schedule", "enabled", "status": "saved"}`. A refused value returns `422` with the reason, an unknown job on `run`, `runs` or `DELETE` returns `404`.

### Command-line tool

| Command | Purpose |
| --- | --- |
| `platformctl job list PROJECT/SERVICE` | Scheduled jobs and their last run. |
| `platformctl job run PROJECT/SERVICE JOB [--wait]` | Run a job now; `--wait` prints its log and exit status. |
| `platformctl job logs PROJECT/SERVICE JOB` | Log of the job's last run. |

### Environment variables

| Variable | Value |
| --- | --- |
| `PLATFORM_JOB` | The job ID. |
| `PLATFORM_JOB_RUN` | The run ID. |

### Behind the scenes

- The service must have a running deployment; a job always uses the image deployed at that moment.
- A job never overlaps itself.
- The job gets the service's CPU and memory, counted in your usage for the minutes it runs.

## Troubleshooting

### The job does not start

The service must have a running deployment. Check that the job is not paused (**resume**) and that the schedule is in UTC.

### Runs are skipped

The previous run was still going when the next one was due. Shorten the command, or schedule it less often.

### Runs are timed out

The command took longer than the **Time limit**. Raise it (up to 1 hour) or make the command faster.

### The command is not found or the shell is missing

The command runs with `/bin/sh -c` in your service's image. Make sure the image has a shell and the program you call.
