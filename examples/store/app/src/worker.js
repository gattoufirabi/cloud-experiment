// Background worker (a service of type "worker": no public address):
//   - reads order events from Kafka and keeps sales statistics in MongoDB;
//   - takes jobs from the platform queue and sends the order confirmation emails.
import { kafka, mongo, queue, sendEmail, money, log } from "./services.js";

if (kafka) {
  const consumer = kafka.consumer({ groupId: "store-worker" });
  try {
    await consumer.connect();
    await consumer.subscribe({ topic: "orders", fromBeginning: false });
    await consumer.run({
      eachMessage: async ({ message }) => {
        const order = JSON.parse(message.value.toString());
        log("order event", { order: order.id, total: order.total_cents });
        if (!mongo) return;
        const day = new Date(order.created_at).toISOString().slice(0, 10);
        await mongo.collection("sales").updateOne(
          { day }, { $inc: { orders: 1, revenue_cents: order.total_cents } }, { upsert: true });
      },
    });
  } catch (e) {
    log("kafka consumer unavailable", { err: e.message });
  }
}

async function emails() {
  if (!queue) return;
  for (;;) {
    let msgs = [];
    try {
      msgs = await queue.receive(10);
    } catch (e) {
      log("queue receive", { err: e.message });
    }
    for (const m of msgs) {
      try {
        const job = JSON.parse(m.body);
        if (job.type === "order-confirmation") {
          const o = job.order;
          const lines = o.items.map((i) => `  ${i.qty} x ${i.name}  ${money(i.price_cents * i.qty)}`).join("\n");
          const id = await sendEmail(o.email, `Your order #${o.id}`,
            `Thank you for your order!\n\n${lines}\n\nTotal: ${money(o.total_cents)}\n\nWe will email you again when it ships.`,
            `<p>Thank you for your order!</p><pre>${lines}</pre><p><strong>Total: ${money(o.total_cents)}</strong></p>`);
          log("confirmation emailed", { order: o.id, email_id: id });
        }
        await queue.done(m.receipt);
      } catch (e) {
        log("job failed, it comes back after the visibility timeout", { err: e.message });
      }
    }
    if (msgs.length === 0) await new Promise((r) => setTimeout(r, 3000));
  }
}
emails();
log("worker started", { kafka: !!kafka, mongodb: !!mongo, queue: !!queue });
// A worker has no port: keep the process alive.
setInterval(() => {}, 1 << 30);
