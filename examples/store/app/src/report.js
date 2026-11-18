// Scheduled job (Jobs tab of the api service): yesterday's sales, emailed to REPORT_TO.
//   schedule: 0 7 * * *     command: node src/report.js
import { db, sendEmail, money, log } from "./services.js";

const { rows } = await db.query(`
  SELECT count(*)::int AS orders, coalesce(sum(total_cents), 0)::int AS revenue,
         count(*) FILTER (WHERE status = 'paid')::int AS paid
  FROM orders WHERE created_at >= current_date - 1 AND created_at < current_date`);
const r = rows[0];
const text = `Sales of yesterday\n\nOrders: ${r.orders} (${r.paid} paid)\nRevenue: ${money(r.revenue)}\n`;
log("daily report", r);
if (process.env.REPORT_TO) await sendEmail(process.env.REPORT_TO, "Store: yesterday's sales", text);
await db.end();
process.exit(0);
