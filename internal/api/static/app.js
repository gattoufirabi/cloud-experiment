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

// ---------- tabs ----------
// Panes are plain sections with ids; tab links point at them (#settings). Without
// JavaScript every pane is shown in order. With it, one pane is visible at a time, the
// choice is kept in the URL fragment, and redirects back to "#tab" reopen it.
const tablist = document.querySelector('.tabs[role="tablist"]');
const paneVisible = (node) => !node.closest(".pane") || !node.closest(".pane").hidden;
if (tablist) {
  const links = [...tablist.querySelectorAll('a[role="tab"]')].filter((a) => a.hash && document.getElementById(a.hash.slice(1)));
  const show = (id, remember) => {
    const current = links.find((a) => a.hash === "#" + id) || links[0];
    if (!current) return;
    links.forEach((a) => {
      const on = a === current;
      a.setAttribute("aria-selected", on ? "true" : "false");
      a.tabIndex = on ? 0 : -1;
      document.getElementById(a.hash.slice(1)).hidden = !on;
    });
    if (remember) history.replaceState(null, "", location.pathname + location.search + current.hash);
    document.dispatchEvent(new CustomEvent("tabshown", { detail: current.hash.slice(1) }));
  };
  tablist.addEventListener("click", (e) => {
    const a = e.target.closest('a[role="tab"]');
    if (!a || !links.includes(a)) return;
    e.preventDefault();
    show(a.hash.slice(1), true);
  });
  tablist.addEventListener("keydown", (e) => { // arrow keys move between tabs
    const i = links.indexOf(document.activeElement);
    if (i < 0 || (e.key !== "ArrowRight" && e.key !== "ArrowLeft")) return;
    const next = links[(i + (e.key === "ArrowRight" ? 1 : links.length - 1)) % links.length];
    next.focus();
    show(next.hash.slice(1), true);
  });
  show(location.hash.slice(1) || tablist.dataset.default || "", false);
  window.addEventListener("hashchange", () => show(location.hash.slice(1), false));
  // The browser jumps to the #pane anchor after load; keep the page header in view instead.
  window.scrollTo(0, 0);
  window.addEventListener("load", () => { if (links.some((a) => a.hash === location.hash)) window.scrollTo(0, 0); });
}

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
  // Poll only while the logs pane is open.
  const loop = () => { clearTimeout(timer); if (paneVisible(logs)) load(); timer = setTimeout(loop, 4000); };
  search.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(loop, 400); });
  document.addEventListener("tabshown", (e) => { if (e.detail === logs.closest(".pane")?.id) loop(); });
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
    const pts = points.map((p) => [((p[0] - t0) / (t1 - t0 || 1)) * 300, 70 - (p[1] / top) * 66]);
    const line = pts.map((p, i) => (i ? "L" : "M") + p[0].toFixed(1) + " " + p[1].toFixed(1)).join("");
    const ns = "http://www.w3.org/2000/svg";
    const area = document.createElementNS(ns, "path");
    area.setAttribute("class", "area");
    area.setAttribute("d", line + "L300 72L0 72Z");
    const path = document.createElementNS(ns, "path");
    path.setAttribute("class", "line");
    path.setAttribute("d", line);
    svg.append(area, path);
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
  const busy = () => deps.querySelector(".st-pending, .st-deploying") !== null;
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

// ---------- docs: highlight the current section in the left pane ----------
const toc = document.querySelector(".docs-toc");
if (toc) {
  const links = [...toc.querySelectorAll('a[href^="#"]')];
  const heads = links.map((a) => document.getElementById(a.hash.slice(1))).filter(Boolean);
  let ticking = false;
  const mark = () => {
    ticking = false;
    let current = heads[0];
    for (const h of heads) if (h.getBoundingClientRect().top <= 140) current = h; // last heading above the reading line
    links.forEach((a) => a.classList.toggle("cur", current && a.hash === "#" + current.id));
  };
  window.addEventListener("scroll", () => { if (!ticking) { ticking = true; requestAnimationFrame(mark); } }, { passive: true });
  // The browser may jump to #section before the page is laid out; jump again once loaded.
  window.addEventListener("load", () => {
    const target = location.hash && document.getElementById(location.hash.slice(1));
    if (target) target.scrollIntoView();
    mark();
  });
  mark();
}
