// Entry point: ROLE chooses which part of the store this container runs.
//   web    - the storefront (public); forwards /api/* to the API over the private network
//   api    - catalogue, carts and orders (private service)
//   worker - background work: order events from Kafka, emails from the queue
const role = process.env.ROLE || "web";
const parts = { web: "./web.js", api: "./api.js", worker: "./worker.js" };
if (!parts[role]) {
  console.error(`unknown ROLE ${role}: use web, api or worker`);
  process.exit(1);
}
await import(parts[role]);
