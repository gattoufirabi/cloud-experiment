// Small progressive enhancements. The console works without JavaScript except for
// live logs, metrics and deployment status refresh. No inline scripts (strict CSP).
"use strict";

// Confirmation for destructive forms.
document.addEventListener("submit", (e) => {
  const msg = e.target.getAttribute("data-confirm");
  if (msg && !window.confirm(msg)) e.preventDefault();
});

const getJSON = async (url) => {
  const r = await fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" } });
  if (!r.ok) throw new Error(r.status);
  return r.json();
};

const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

// ---------- logs ----------
const logs = document.getElementById("logs");
if (logs) {
  const search = document.getElementById("log-search");
  const follow = document.getElementById("log-follow");
  let timer;
  const load = async () => {
    try {
      const q = search.value.trim();
      const lines = await getJSON(logs.dataset.url + (q ? "?q=" + encodeURIComponent(q) : ""));
      const atBottom = logs.scrollTop + logs.clientHeight >= logs.scrollHeight - 20;
      logs.replaceChildren();
      if (lines.length === 0) logs.textContent = "No log lines in the last 6 hours.";
      for (const l of lines) {
        const row = el("div", l.stream === "stderr" ? "err" : "");
        row.append(el("time", "", (l.time || "").replace("T", " ").slice(0, 19) + "  "), document.createTextNode(l.msg));
        logs.append(row);
      }
      if (follow.checked && (atBottom || lines.length)) logs.scrollTop = logs.scrollHeight;
    } catch (_) {
      logs.textContent = "Logs are not available right now.";
    }
  };
  const loop = () => { clearTimeout(timer); load(); timer = setTimeout(loop, 4000); };
  search.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(loop, 400); });
  loop();
}

// ---------- metrics ----------
const metrics = document.getElementById("metrics");
if (metrics) {
  const draw = (svg, points, max) => {
    svg.replaceChildren();
    if (points.length < 2) return;
    const t0 = points[0][0], t1 = points[points.length - 1][0];
    const top = Math.max(max, ...points.map((p) => p[1])) || 1;
    const d = points.map((p, i) => {
      const x = ((p[0] - t0) / (t1 - t0 || 1)) * 300;
      const y = 58 - (p[1] / top) * 56;
      return (i ? "L" : "M") + x.toFixed(1) + " " + y.toFixed(1);
    }).join("");
    const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
    path.setAttribute("d", d);
    svg.append(path);
  };
  const load = async () => {
    try {
      const m = await getJSON(metrics.dataset.url);
      const has = m.cpu.length > 1 || m.memory.length > 1;
      metrics.querySelector("[data-empty]").hidden = has;
      draw(metrics.querySelector('[data-chart="cpu"]'), m.cpu, 10);
      draw(metrics.querySelector('[data-chart="memory"]'), m.memory, m.limit_mb || 1);
      const last = (a) => (a.length ? a[a.length - 1][1] : null);
      const c = last(m.cpu), mem = last(m.memory);
      metrics.querySelector('[data-last="cpu"]').textContent = c === null ? "" : c.toFixed(1) + " % of a core";
      metrics.querySelector('[data-last="memory"]').textContent = mem === null ? "" : mem.toFixed(0) + " / " + m.limit_mb + " MB";
    } catch (_) { /* keep last drawing */ }
  };
  load();
  setInterval(load, 30000);
}

// ---------- deployment status: reload while a deployment is in progress ----------
const deps = document.getElementById("deployments");
if (deps) {
  const busy = () => deps.querySelector(".s-pending, .s-deploying") !== null;
  if (busy()) {
    const poll = async () => {
      try {
        const list = await getJSON(deps.dataset.statusUrl);
        const latest = list[0];
        if (latest && latest.Status !== "pending" && latest.Status !== "deploying") {
          window.location.reload();
          return;
        }
        const cell = deps.querySelector("tbody tr td:nth-child(2)");
        if (cell && latest) {
          const pre = cell.querySelector("pre.msg") || cell.appendChild(el("pre", "msg"));
          pre.textContent = latest.Message;
        }
      } catch (_) { /* retry */ }
      setTimeout(poll, 2500);
    };
    setTimeout(poll, 2500);
  }
}
