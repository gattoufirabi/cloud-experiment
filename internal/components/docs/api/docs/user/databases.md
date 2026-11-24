# Managed databases

A managed database runs next to your services on the project's private network, with nightly encrypted backups. You do not install, secure or back up a database server yourself.

## Overview

Each database is a container started by the platform inside one project, with its own persistent volume. Only services of the same project can reach it, so traffic stays on the private network. You pick an engine and a version, and the platform creates the database `app` and the user `app` for you.

Six engines are offered:

| Engine | Versions | Port | Secret name when attached |
| --- | --- | --- | --- |
| PostgreSQL | 17, 16 | 5432 | `DATABASE_URL` |
| MySQL | 8.4 | 3306 | `DATABASE_URL` |
| MariaDB | 11.4 | 3306 | `DATABASE_URL` |
| MongoDB | 8.0 | 27017 | `MONGODB_URI` |
| Redis | 7 | 6379 | `REDIS_URL` |
| Apache Kafka (streaming) | 3.9 | 9092 | `KAFKA_URL` |

Managed databases count against your plan, all engines together: Trial and Starter include 1, Pro includes 5 (default plans). Their CPU and memory share the plan's totals with your services, and their data and backup dumps count against your disk limit.

## Use cases

- **Application database**: create a PostgreSQL, MySQL or MariaDB database and attach it to your web service with **Add secret**. Your code reads `DATABASE_URL`.
- **Cache or sessions**: create a Redis database and read `REDIS_URL`. Size its memory for your data, because Redis refuses writes when full.
- **Document store**: use MongoDB for JSON-like documents, read through `MONGODB_URI`.
- **Event stream between services**: use Kafka for events that several services read, through `KAFKA_URL`.
- **Reporting from your laptop**: open **Public access** on PostgreSQL, Redis or MongoDB with an allow-list so a reporting tool can connect over TLS.
- **Safe migrations**: take **Back up now** before a risky migration, and **Restore** if it goes wrong.
- **Scale up**: raise **CPU** and **Memory** on the **Settings** tab when the database gets busy.

## How to use it

### Create a database

1. Open **Databases** and click **Create database** (developers), or use the **Create a database** form on a project's **Databases** tab.
2. Choose the **Project**, a **Database ID** (2 to 20 lowercase letters, digits and dashes, such as `main`) and the **Engine**.
3. Click **Create database**. It starts in under a minute (the activity indicator shows it) with 250 millicores and 256 MB (512 MB for MongoDB and Kafka), counted against your plan.

```
POST /api/v1/projects/PROJECT/databases
{"slug": "main", "engine": "postgres:17"}
```

### Find and manage your databases

The **Databases** list shows every database with its project, engine and status. Use the search box to filter. Select several rows to use the bulk actions **Back up now** and **Delete**.

A database page has four tabs:

- **Overview**: status, who can reach it and the backup schedule.
- **Connect**: connection details, **Add to a service** and **Public access**.
- **Backups**: **Back up now**, **Snapshots** and **Recent jobs**.
- **Settings**: backup schedule, **CPU**, **Memory** and deletion.

### Connect a service

1. Open the database's **Connect** tab, section **Add to a service**.
2. Choose a service of the same project and keep the suggested secret name (or type another, in capitals).
3. Click **Add secret**. The connection URL is stored as an encrypted secret of that service.
4. Redeploy the service to apply it, then use the variable from your application.

```
POST /api/v1/projects/PROJECT/databases/DB/attach
{"service": "web", "name": "DATABASE_URL"}
```

The **Connect** tab also shows the **Connection details** one by one (host, port, database and user, and for developers the password and the full **Connection URL**), for tools that want separate fields. Developers and above see the password. Viewers see the host, port, database and user only.

### Open public access

Public access opens a database to tools outside the platform (your laptop, a reporting tool, a server in another cloud) while keeping everyone else out. It is available for PostgreSQL, Redis and MongoDB, and for admins only.

1. On the database's **Connect** tab, section **Public access**, list the addresses allowed to connect, separated by commas (IP addresses or ranges such as `198.51.100.0/24`). The field is prefilled with your current address.
2. Click **Open public access**.
3. Save the certificate shown under **Certificate (save as db.crt)**.
4. Connect to the host `<db>-<project>-<organization>-db.{{apps}}` on the port shown. **Example** gives a ready command.
5. Click **Update the allow-list** to change the addresses, or **Close public access** to remove the endpoint.

```
psql "postgres://app:PASSWORD@main-shop-acme-db.{{apps}}:PORT/app?sslmode=verify-full&sslrootcert=db.crt"
redis-cli -u "rediss://default:PASSWORD@cache-shop-acme-db.{{apps}}:PORT/0" --cacert db.crt
mongosh "mongodb://app:PASSWORD@docs-shop-acme-db.{{apps}}:PORT/app?authSource=admin&tls=true&tlsCAFile=db.crt"
```

```
POST /api/v1/projects/PROJECT/databases/DB/public
{"allow": "203.0.113.7"}
```

To close it, send `{"close": true}` to the same path.

### Schedule and take backups

Every database except Kafka is backed up every night.

1. Open the **Settings** tab, keep **nightly backup** ticked and choose the **Backup hour** (UTC). Click **Save settings**.
2. To take a backup right now, open the **Backups** tab and click **Back up now** (developers). The bulk action on the **Databases** list does it for several databases.
3. Read the **Snapshots** list: it shows when each snapshot was taken and its size, with **nightly** and **before restore** badges. **Recent jobs** lists the latest backups and restores with their result.

Only one backup or restore runs at a time per database.

### Restore a snapshot

1. Open the **Backups** tab (admins).
2. Next to the snapshot you want, type the database ID and click **Restore**.
3. Wait a few seconds. Connected applications may see errors while it runs.

A safety snapshot of the current data is taken first and always kept. See [Behind the scenes](#behind-the-scenes) for what each engine does on restore.

### Change resources

1. Open the **Settings** tab.
2. Set **CPU** (50 to 4000 millicores) and **Memory** (128 to 8192 MB; at least 512 MB for MongoDB and Kafka).
3. Click **Save settings**. The database restarts with the new values (a few seconds of downtime). Data is kept in its persistent volume.

### Delete a database

1. At the bottom of the **Settings** tab, type the database ID (admins).
2. Click **Delete database**.

The database stops and leaves the project. Its data volume is kept by your platform operator until it is cleaned up (see [the admin guide](/docs/admin)). Take a backup first if you may need the data.

## Technical reference

### Limits and plans

| Item | Limit / default | Notes |
| --- | --- | --- |
| Database ID | 2 to 20 lowercase letters, digits, dashes | For example `main`. |
| Databases per organization | Trial 1, Starter 1, Pro 5 | Default plans, all engines together. |
| Starting resources | 250 millicores, 256 MB | 512 MB for MongoDB and Kafka. |
| CPU | 50 to 4000 millicores | Shares the plan's totals with your services. |
| Memory | 128 to 8192 MB | At least 512 MB for MongoDB and Kafka. |
| Public ports | Counted against your plan | None on Trial. |
| Disk | Data and backup dumps count against your disk limit | |

### Connection strings

The host `pd-…` only resolves inside the project's network.

| Engine | Connection string |
| --- | --- |
| PostgreSQL | `postgres://app:…@pd-…:5432/app?sslmode=disable` |
| MySQL, MariaDB | `mysql://app:…@pd-…:3306/app` |
| MongoDB | `mongodb://app:…@pd-…:27017/app?authSource=admin` |
| Redis | `redis://default:…@pd-…:6379/0` |
| Kafka | `kafka://app:…@pd-…:9092?security.protocol=SASL_PLAINTEXT&sasl.mechanism=PLAIN` |

### Engine notes

- **PostgreSQL:** the database `app` and user `app` are created for you.
- **MySQL and MariaDB:** the database `app` and user `app` are created for you. `root` gets a random password that nobody is given. InnoDB gets half the memory you give MariaDB, a quarter for MySQL (which needs more for itself), so give MySQL at least 256 MB. Up to 100 connections are accepted. Named time zones (`CONVERT_TZ(..., 'Europe/Paris')`) are not loaded: use offsets such as `+01:00` or UTC.
- **MongoDB:** the database `app` and user `app` (an administrator of this server only) are created for you. It starts with 512 MB, its minimum. The storage engine's cache gets 40% of the memory (0.25 GB at least).
- **Redis:** uses at most 80% of the memory you give it, then refuses new writes instead of evicting keys. Size it for your data.
- **Apache Kafka:** a single-broker Kafka (KRaft mode, no separate coordination service) on the project's private network. Give your Kafka client the bootstrap server `pd-…:9092`, security protocol `SASL_PLAINTEXT`, mechanism `PLAIN`, user `app` and the password (all shown on the **Connect** tab). Topics are created on first use and messages are kept for 7 days. It needs at least 512 MB (60% for the JVM heap). Kafka is **not backed up**: keep data you need in a database or a bucket.

### Public access rules

- TLS is required, with the database's own certificate: pass it as `sslrootcert` for `sslmode=verify-full`, `--cacert` for Redis, `tlsCAFile` for MongoDB. Plain-text connections are refused.
- Connections from any other address are dropped before they reach the database. Avoid `0.0.0.0/0`, which would allow everyone.
- MySQL, MariaDB and Kafka negotiate TLS inside their own protocols and have no public endpoint yet: connect from a service of the project.
- The public port counts against your plan's public ports (none on Trial).
- The allow-list takes up to 20 addresses or ranges. The certificate is generated for the database when you first open public access and is valid for 10 years.

### API

| Method | Path | Role | Purpose |
| --- | --- | --- | --- |
| POST | `/api/v1/projects/PROJECT/databases` | developer | Create a database. |
| POST | `/api/v1/projects/PROJECT/databases/DB/attach` | developer | Store the connection URL as a service secret. |
| POST | `/api/v1/projects/PROJECT/databases/DB/public` | admin | Open, update or close public access. |

These calls answer `{"message": "…"}`, or `422` with `{"error": "…"}` when refused (an ID already used, a plan limit, an unknown service). Resources, backups, restores and deletion are done in the console only.

Create a database:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `slug` | string | yes | The Database ID: 2 to 20 lowercase letters, digits and dashes. |
| `engine` | string | no | `postgres:17`, `postgres:16`, `mysql:8.4`, `mariadb:11.4`, `mongodb:8.0`, `redis:7` or `kafka:3.9`. An empty or unknown value creates `postgres:17`. |

Attach:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `service` | string | yes | A service of the same project. |
| `name` | string | no | Secret name in capitals, digits and underscores (up to 64), not starting with `PLATFORM_`. Defaults to the engine's usual name (`DATABASE_URL`, `MONGODB_URI`, `REDIS_URL` or `KAFKA_URL`). |

Public access:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `allow` | string | to open or update | Allowed addresses or ranges, such as `203.0.113.7` or `198.51.100.0/24`, separated by commas, spaces or new lines (at most 20). |
| `close` | boolean | to close | `true` removes the public endpoint. |

### Behind the scenes

- **Backup tool:** a consistent `pg_dump` (PostgreSQL), `mysqldump` / `mariadb-dump` (MySQL, MariaDB), `mongodump` (MongoDB) or snapshot (Redis). The dump is encrypted with your organization's key.
- **Retention:** 7 daily, 4 weekly and 6 monthly snapshots.
- **Restore, PostgreSQL:** loaded with `pg_restore`, replacing existing tables.
- **Restore, MySQL and MariaDB:** every table in the snapshot is recreated. Tables created after it are left in place.
- **Restore, MongoDB:** `mongorestore --drop` replaces each collection in the snapshot. Collections created after it are left in place.
- **Restore, Redis:** Redis is briefly stopped and restarted with the snapshot.
- **Deleting:** the volume is kept by the operator until cleaned up.
