// The storefront (public web service): static pages, and /api/* forwarded to the API
// over the private network (API_URL, such as http://api.shop.internal). No dependency.
import http from "node:http";
import fs from "node:fs/promises";
import path from "node:path";

const api = process.env.API_URL || "http://api.shop.internal";
const root = path.resolve("public");
const types = { ".html": "text/html; charset=utf-8", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml" };

http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  if (url.pathname.startsWith("/api/")) {
    try {
      const body = req.method === "GET" || req.method === "HEAD" ? undefined : await new Promise((ok) => {
        const chunks = [];
        req.on("data", (c) => chunks.push(c)).on("end", () => ok(Buffer.concat(chunks)));
      });
      const r = await fetch(api + url.pathname + url.search, {
        method: req.method, body, headers: { "Content-Type": req.headers["content-type"] || "application/json" },
      });
      res.writeHead(r.status, { "Content-Type": r.headers.get("content-type") || "application/json" });
      res.end(Buffer.from(await r.arrayBuffer()));
    } catch (e) {
      res.writeHead(502, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "the API is not reachable: " + e.message }));
    }
    return;
  }
  const file = path.join(root, url.pathname === "/" ? "index.html" : path.normalize(url.pathname));
  if (!file.startsWith(root)) {
    res.writeHead(400);
    return res.end();
  }
  try {
    const data = await fs.readFile(file);
    res.writeHead(200, { "Content-Type": types[path.extname(file)] || "application/octet-stream", "Cache-Control": "public, max-age=60" });
    res.end(data);
  } catch {
    res.writeHead(404, { "Content-Type": "text/plain" });
    res.end("not found\n");
  }
}).listen(Number(process.env.PORT || 8080), () => console.log(JSON.stringify({ msg: "storefront listening", api })));
