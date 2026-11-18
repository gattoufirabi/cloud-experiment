// The store's API: a private service (no public address). The storefront reaches it at
// http://api.<project>.internal; a custom domain can also route /api to it.
import http from "node:http";
import crypto from "node:crypto";
import { db, migrate, redis, mongo, kafka, s3, uploadURL, imageURL, queue, log } from "./services.js";

await migrate();
const producer = kafka ? kafka.producer() : null;
if (producer) await producer.connect().catch((e) => log("kafka unavailable", { err: e.message }));

// Prometheus metrics: the platform scrapes /metrics when the service's metrics path is set.
const metrics = { requests: 0, orders: 0, revenueCents: 0, cacheHits: 0, cacheMisses: 0 };

const send = (res, status, body) => {
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify(body));
};
async function readJSON(req) {
  let raw = "";
  for await (const chunk of req) {
    raw += chunk;
    if (raw.length > 64 * 1024) throw new Error("body too large");
  }
  return raw ? JSON.parse(raw) : {};
}
// Calls from the store's own functions (photo uploads, payments) carry INTERNAL_TOKEN.
const internal = (req) => {
  const want = process.env.INTERNAL_TOKEN || "";
  const got = req.headers["x-internal-token"] || "";
  return want !== "" && got.length === want.length && crypto.timingSafeEqual(Buffer.from(got), Buffer.from(want));
};

async function products() {
  if (redis) {
    const hit = await redis.get("products");
    if (hit) { metrics.cacheHits++; return JSON.parse(hit); }
    metrics.cacheMisses++;
  }
  const { rows } = await db.query("SELECT id, name, description, price_cents, stock, image_key FROM products ORDER BY id");
  for (const p of rows) p.image = p.image_key && s3 ? await imageURL(p.image_key) : null;
  if (redis) await redis.set("products", JSON.stringify(rows), { EX: 30 });
  return rows;
}

const routes = [
  ["GET", /^\/api\/health$/, async () => [200, { ok: true, postgres: !!db, redis: !!redis, mongodb: !!mongo, kafka: !!producer, s3: !!s3, queue: !!queue }]],
  ["GET", /^\/api\/products$/, async () => [200, await products()]],
  ["GET", /^\/api\/products\/(\d+)\/reviews$/, async (req, m) => {
    if (!mongo) return [200, []];
    return [200, await mongo.collection("reviews").find({ product: Number(m[1]) }).sort({ at: -1 }).limit(20).toArray()];
  }],
  ["POST", /^\/api\/products\/(\d+)\/reviews$/, async (req, m) => {
    const b = await readJSON(req);
    const stars = Math.max(1, Math.min(5, Number(b.stars) || 5));
    const text = String(b.text || "").slice(0, 500);
    if (!mongo || !text) return [400, { error: "a review needs a text" }];
    await mongo.collection("reviews").insertOne({ product: Number(m[1]), stars, text, name: String(b.name || "Anonymous").slice(0, 60), at: new Date() });
    return [201, { ok: true }];
  }],
  ["POST", /^\/api\/orders$/, async (req) => {
    const b = await readJSON(req);
    const email = String(b.email || "").trim();
    const items = Array.isArray(b.items) ? b.items.slice(0, 20) : [];
    if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(email) || items.length === 0) return [400, { error: "an email address and at least one item" }];
    const client = await db.connect();
    try {
      await client.query("BEGIN");
      let total = 0;
      const lines = [];
      for (const it of items) {
        const qty = Math.max(1, Math.min(10, Number(it.qty) || 1));
        const { rows } = await client.query("UPDATE products SET stock = stock - $2 WHERE id = $1 AND stock >= $2 RETURNING id, name, price_cents", [Number(it.id), qty]);
        if (!rows[0]) throw Object.assign(new Error(`product ${it.id} is out of stock`), { status: 409 });
        total += rows[0].price_cents * qty;
        lines.push({ id: rows[0].id, name: rows[0].name, qty, price_cents: rows[0].price_cents });
      }
      const { rows } = await client.query("INSERT INTO orders (email, items, total_cents) VALUES ($1, $2, $3) RETURNING id, created_at", [email, JSON.stringify(lines), total]);
      await client.query("COMMIT");
      const order = { id: rows[0].id, email, items: lines, total_cents: total, created_at: rows[0].created_at };
      metrics.orders++;
      metrics.revenueCents += total;
      if (redis) await redis.del("products");
      // the worker takes it from here: an event on Kafka, the confirmation email through the queue
      if (producer) await producer.send({ topic: "orders", messages: [{ key: String(order.id), value: JSON.stringify(order) }] }).catch((e) => log("kafka send", { err: e.message }));
      if (queue) await queue.send({ type: "order-confirmation", order }).catch((e) => log("queue send", { err: e.message }));
      return [201, order];
    } catch (e) {
      await client.query("ROLLBACK");
      if (e.status) return [e.status, { error: e.message }];
      throw e;
    } finally {
      client.release();
    }
  }],
  ["GET", /^\/api\/orders\/(\d+)$/, async (req, m) => {
    const { rows } = await db.query("SELECT id, items, total_cents, status, created_at, paid_at FROM orders WHERE id = $1", [Number(m[1])]);
    return rows[0] ? [200, rows[0]] : [404, { error: "no such order" }];
  }],
  // a signed upload URL for a product photo (the bucket trigger then calls /api/internal/image)
  ["POST", /^\/api\/products\/(\d+)\/image-upload$/, async (req, m) => {
    if (!s3) return [503, { error: "object storage is not connected" }];
    const key = `products/${m[1]}/${Date.now()}.jpg`;
    return [200, { url: await uploadURL(key, "image/jpeg"), key }];
  }],
  ["POST", /^\/api\/internal\/image$/, async (req) => {
    if (!internal(req)) return [403, { error: "forbidden" }];
    const b = await readJSON(req);
    const m = /^products\/(\d+)\//.exec(String(b.key || ""));
    if (!m) return [400, { error: "not a product photo" }];
    await db.query("UPDATE products SET image_key = $2 WHERE id = $1", [Number(m[1]), b.key]);
    if (redis) await redis.del("products");
    return [200, { ok: true }];
  }],
  ["POST", /^\/api\/internal\/paid$/, async (req) => {
    if (!internal(req)) return [403, { error: "forbidden" }];
    const b = await readJSON(req);
    const { rowCount } = await db.query("UPDATE orders SET status = 'paid', paid_at = now() WHERE id = $1 AND status = 'pending'", [Number(b.order)]);
    return [200, { updated: rowCount }];
  }],
];

http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  if (url.pathname === "/metrics") {
    res.writeHead(200, { "Content-Type": "text/plain; version=0.0.4" });
    res.end([
      "# TYPE store_requests_total counter", `store_requests_total ${metrics.requests}`,
      "# TYPE store_orders_total counter", `store_orders_total ${metrics.orders}`,
      "# TYPE store_revenue_cents_total counter", `store_revenue_cents_total ${metrics.revenueCents}`,
      "# TYPE store_cache_hits_total counter", `store_cache_hits_total ${metrics.cacheHits}`,
      "# TYPE store_cache_misses_total counter", `store_cache_misses_total ${metrics.cacheMisses}`, "",
    ].join("\n"));
    return;
  }
  if (url.pathname === "/") return send(res, 200, { service: "store api" }); // health check
  metrics.requests++;
  for (const [method, re, fn] of routes) {
    const m = re.exec(url.pathname);
    if (m && req.method === method) {
      try {
        const [status, body] = await fn(req, m);
        return send(res, status, body);
      } catch (e) {
        log("request failed", { path: url.pathname, err: e.message });
        return send(res, 500, { error: "internal error" });
      }
    }
  }
  send(res, 404, { error: "not found" });
}).listen(Number(process.env.PORT || 8080), () => log("api listening"));
