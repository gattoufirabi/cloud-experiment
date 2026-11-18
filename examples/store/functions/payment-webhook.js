// Function behind a webhook URL: the payment provider calls it when an order is paid.
// It checks the provider's signature (HMAC-SHA256 of the body with PAYMENT_SECRET, in the
// X-Signature header), then marks the order paid through the API.
import crypto from "node:crypto";

export const handler = async (event) => {
  const body = event.isBase64Encoded ? Buffer.from(event.body || "", "base64").toString() : event.body || "";
  const secret = process.env.PAYMENT_SECRET || "";
  const want = crypto.createHmac("sha256", secret).update(body).digest("hex");
  const got = (event.headers && (event.headers["x-signature"] || event.headers["X-Signature"])) || "";
  if (!secret || got.length !== want.length || !crypto.timingSafeEqual(Buffer.from(got), Buffer.from(want))) {
    console.log("payment webhook: bad signature");
    return { statusCode: 401, body: "bad signature" };
  }
  const { order, status } = JSON.parse(body);
  if (status !== "paid") return { statusCode: 200, body: "ignored" };
  const res = await fetch(`${process.env.API_URL || "http://api.shop.internal"}/api/internal/paid`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Internal-Token": process.env.INTERNAL_TOKEN || "" },
    body: JSON.stringify({ order }),
  });
  console.log("order", order, "paid ->", res.status);
  return { statusCode: 200, body: JSON.stringify({ order, updated: res.ok }) };
};
