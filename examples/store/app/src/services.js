// Connections to the platform's managed services, all configured by secrets and variables
// the platform writes into the service ("Add secret" / "Connect to a service"):
//   DATABASE_URL (PostgreSQL)   REDIS_URL (Redis)   MONGODB_URI (MongoDB)   KAFKA_URL (Kafka)
//   AWS_* + S3_BUCKET (object storage)   QUEUE_URL + QUEUE_TOKEN (queue)
//   EMAIL_API_URL + EMAIL_TOKEN (email)
// Each one is optional: the store keeps working (with less) when a service is not connected.
import pg from "pg";
import { createClient } from "redis";
import { MongoClient } from "mongodb";
import { Kafka, logLevel } from "kafkajs";
import { S3Client, PutObjectCommand, GetObjectCommand } from "@aws-sdk/client-s3";
import { getSignedUrl } from "@aws-sdk/s3-request-presigner";

const env = process.env;
export const log = (msg, extra = {}) => console.log(JSON.stringify({ time: new Date().toISOString(), msg, ...extra }));

// --- PostgreSQL: products and orders ---
export const db = env.DATABASE_URL ? new pg.Pool({ connectionString: env.DATABASE_URL, max: 5 }) : null;

export async function migrate() {
  if (!db) return;
  await db.query(`
    CREATE TABLE IF NOT EXISTS products (
      id serial PRIMARY KEY, name text NOT NULL, description text NOT NULL DEFAULT '',
      price_cents int NOT NULL, stock int NOT NULL DEFAULT 0, image_key text);
    CREATE TABLE IF NOT EXISTS orders (
      id serial PRIMARY KEY, email text NOT NULL, items jsonb NOT NULL, total_cents int NOT NULL,
      status text NOT NULL DEFAULT 'pending', created_at timestamptz NOT NULL DEFAULT now(), paid_at timestamptz)`);
  const { rows } = await db.query("SELECT count(*)::int AS n FROM products");
  if (rows[0].n === 0) {
    await db.query(`INSERT INTO products (name, description, price_cents, stock) VALUES
      ('Ceramic mug', 'Hand-glazed, 350 ml, dishwasher safe.', 1800, 40),
      ('Linen tote bag', 'Natural linen, reinforced handles.', 2400, 25),
      ('Notebook A5', 'Dot grid, 160 pages, lay-flat binding.', 1200, 60),
      ('Desk plant', 'Low-light pothos in a terracotta pot.', 2900, 12),
      ('Wool socks', 'Merino blend, made to last.', 1500, 80),
      ('Coffee beans 250 g', 'Medium roast, notes of cocoa and cherry.', 1100, 50)`);
    log("catalogue seeded");
  }
}

// --- Redis: cache of the catalogue ---
export let redis = null;
if (env.REDIS_URL) {
  redis = createClient({ url: env.REDIS_URL });
  redis.on("error", (e) => log("redis error", { err: e.message }));
  await redis.connect().catch((e) => { log("redis unavailable", { err: e.message }); redis = null; });
}

// --- MongoDB: product reviews and order statistics ---
export let mongo = null;
if (env.MONGODB_URI) {
  const client = new MongoClient(env.MONGODB_URI, { serverSelectionTimeoutMS: 5000 });
  await client.connect().then(() => { mongo = client.db(); }).catch((e) => log("mongodb unavailable", { err: e.message }));
}

// --- Kafka: order events (kafka://user:password@host:9092?security.protocol=SASL_PLAINTEXT&sasl.mechanism=PLAIN) ---
export let kafka = null;
if (env.KAFKA_URL) {
  const u = new URL(env.KAFKA_URL);
  kafka = new Kafka({
    clientId: env.ROLE || "store", brokers: [u.host], logLevel: logLevel.WARN,
    sasl: { mechanism: "plain", username: decodeURIComponent(u.username), password: decodeURIComponent(u.password) },
  });
}

// --- Object storage: product photos ---
export const s3 = env.S3_BUCKET ? new S3Client({
  region: env.AWS_REGION, endpoint: env.AWS_ENDPOINT_URL_S3 || env.AWS_ENDPOINT_URL, forcePathStyle: true,
}) : null;

export async function uploadURL(key, contentType) {
  return getSignedUrl(s3, new PutObjectCommand({ Bucket: env.S3_BUCKET, Key: key, ContentType: contentType }), { expiresIn: 600 });
}

export async function imageURL(key) {
  return getSignedUrl(s3, new GetObjectCommand({ Bucket: env.S3_BUCKET, Key: key }), { expiresIn: 3600 });
}

// --- Queue: jobs for the worker (the platform's queue API) ---
async function queueCall(path, method, body) {
  const r = await fetch(env.QUEUE_URL + path, {
    method, headers: { Authorization: `Bearer ${env.QUEUE_TOKEN}`, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!r.ok) throw new Error(`queue ${method} ${path}: HTTP ${r.status}`);
  return r.status === 204 ? null : r.json();
}
export const queue = env.QUEUE_URL ? {
  send: (job) => queueCall("/messages", "POST", { body: JSON.stringify(job) }),
  receive: async (max = 10) => (await queueCall(`/receive?max=${max}`, "POST")).messages || [],
  done: (receipt) => queueCall(`/messages/${encodeURIComponent(receipt)}`, "DELETE"),
} : null;

// --- Email: the platform's sending API (from a verified domain, EMAIL_FROM) ---
export async function sendEmail(to, subject, text, html) {
  if (!env.EMAIL_API_URL || !env.EMAIL_FROM) {
    log("email not configured, skipped", { to, subject });
    return null;
  }
  const r = await fetch(env.EMAIL_API_URL, {
    method: "POST", headers: { Authorization: `Bearer ${env.EMAIL_TOKEN}`, "Content-Type": "application/json" },
    body: JSON.stringify({ from: env.EMAIL_FROM, to: [to], subject, text, html }),
  });
  if (r.status !== 202) throw new Error(`email: HTTP ${r.status} ${await r.text()}`);
  return (await r.json()).id;
}

export const money = (cents) => (cents / 100).toFixed(2) + " " + (env.CURRENCY || "EUR");
