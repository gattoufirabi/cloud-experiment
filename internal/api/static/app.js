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

/* ---------- passkeys (WebAuthn) ---------- */
(function () {
  var b64u = {
    dec: function (s) {
      s = s.replace(/-/g, "+").replace(/_/g, "/");
      while (s.length % 4) s += "=";
      var bin = atob(s), out = new Uint8Array(bin.length);
      for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
      return out.buffer;
    },
    enc: function (buf) {
      var bytes = new Uint8Array(buf), bin = "";
      for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
      return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
    }
  };
  function post(url, body, csrf) {
    var h = { "Content-Type": "application/json" };
    if (csrf) h["X-CSRF-Token"] = csrf;
    return fetch(url, { method: "POST", headers: h, body: body ? JSON.stringify(body) : "{}", credentials: "same-origin" })
      .then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (j) {
          if (!r.ok) throw new Error(j.error || "request failed (" + r.status + ")");
          return j;
        });
      });
  }
  function showErr(root, e) {
    var p = root.querySelector("[data-passkey-error]");
    if (!p) return;
    p.textContent = e && e.name === "NotAllowedError" ? "Cancelled or timed out." : (e && e.message) || String(e);
    p.hidden = false;
  }
  var supported = !!(window.PublicKeyCredential && navigator.credentials);

  var reg = document.querySelector("[data-passkey-register]");
  if (reg) {
    if (!supported) {
      reg.hidden = true;
      var u = document.querySelector("[data-passkey-unsupported]");
      if (u) u.hidden = false;
    }
    reg.addEventListener("submit", function (ev) {
      ev.preventDefault();
      var csrf = reg.getAttribute("data-csrf"), name = reg.elements.name.value;
      post("/account/passkeys/begin", null, csrf).then(function (opt) {
        var pk = opt.publicKey;
        pk.challenge = b64u.dec(pk.challenge);
        pk.user.id = b64u.dec(pk.user.id);
        (pk.excludeCredentials || []).forEach(function (c) { c.id = b64u.dec(c.id); });
        return navigator.credentials.create({ publicKey: pk });
      }).then(function (cred) {
        return post("/account/passkeys/finish?name=" + encodeURIComponent(name), {
          id: cred.id, rawId: b64u.enc(cred.rawId), type: cred.type,
          response: {
            clientDataJSON: b64u.enc(cred.response.clientDataJSON),
            attestationObject: b64u.enc(cred.response.attestationObject),
            transports: cred.response.getTransports ? cred.response.getTransports() : []
          }
        }, csrf);
      }).then(function () {
        location.href = "/account?msg=" + encodeURIComponent("Passkey added.");
      }).catch(function (e) { showErr(reg, e); });
    });
  }

  var login = document.querySelector("[data-passkey-login]");
  if (login && supported) {
    login.hidden = false;
    login.querySelector("[data-passkey-signin]").addEventListener("click", function () {
      post("/login/passkey/begin").then(function (opt) {
        var pk = opt.publicKey;
        pk.challenge = b64u.dec(pk.challenge);
        (pk.allowCredentials || []).forEach(function (c) { c.id = b64u.dec(c.id); });
        return navigator.credentials.get({ publicKey: pk });
      }).then(function (cred) {
        var r = cred.response;
        return post("/login/passkey/finish", {
          id: cred.id, rawId: b64u.enc(cred.rawId), type: cred.type,
          response: {
            clientDataJSON: b64u.enc(r.clientDataJSON),
            authenticatorData: b64u.enc(r.authenticatorData),
            signature: b64u.enc(r.signature),
            userHandle: r.userHandle ? b64u.enc(r.userHandle) : null
          }
        });
      }).then(function (j) { location.href = j.redirect || "/"; })
        .catch(function (e) { showErr(login, e); });
    });
  }
})();

/* ---------- invoices: print button (no inline scripts under the CSP) ---------- */
document.addEventListener("click", function (e) {
  if (e.target.closest && e.target.closest("[data-print]")) window.print();
});

/* ---------- signup: proof of work (privacy-friendly bot check, no third party) ---------- */
(function () {
  var form = document.querySelector("form[data-pow]");
  if (!form || !window.crypto || !crypto.subtle) return;
  var solving = false;
  function zeroBits(buf) {
    var b = new Uint8Array(buf), n = 0;
    for (var i = 0; i < b.length; i++) {
      if (b[i] === 0) { n += 8; continue; }
      for (var m = 128; m && !(b[i] & m); m >>= 1) n++;
      return n;
    }
    return n;
  }
  form.addEventListener("submit", function (ev) {
    if (form.elements.nonce.value) return;
    ev.preventDefault();
    if (solving) return;
    solving = true;
    var ch = form.elements.challenge.value, need = parseInt(ch.split(".")[2], 10) || 16;
    var status = form.querySelector("[data-pow-status]"), btn = form.querySelector("[data-pow-button]");
    if (status) status.hidden = false;
    if (btn) btn.disabled = true;
    var enc = new TextEncoder(), nonce = 0;
    (function batch() {
      var tries = [];
      for (var i = 0; i < 256; i++, nonce++) {
        (function (n) {
          tries.push(crypto.subtle.digest("SHA-256", enc.encode(ch + ":" + n)).then(function (h) { return zeroBits(h) >= need ? n : -1; }));
        })(nonce);
      }
      Promise.all(tries).then(function (res) {
        for (var j = 0; j < res.length; j++) {
          if (res[j] >= 0) { form.elements.nonce.value = String(res[j]); form.submit(); return; }
        }
        batch();
      });
    })();
  });
})();

// ---------- console shell ----------
// Menus (services, organization, account) are <details>: close them on a click
// outside or Escape. The project filter applies as soon as it changes. Tables with a
// search box above them filter their rows as you type.
document.addEventListener("click", (e) => {
  document.querySelectorAll("details.menu[open]").forEach((d) => { if (!d.contains(e.target)) d.removeAttribute("open"); });
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") document.querySelectorAll("details.menu[open]").forEach((d) => d.removeAttribute("open"));
  if (e.key === "/" && !/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName || "")) {
    const q = document.querySelector(".search input");
    if (q) { e.preventDefault(); q.focus(); }
  }
});
document.querySelectorAll("select[data-autosubmit]").forEach((s) => s.addEventListener("change", () => s.form.submit()));
document.querySelectorAll("input[data-filter]").forEach((input) => {
  const table = document.getElementById(input.dataset.filter);
  if (!table) return;
  input.addEventListener("input", () => {
    const q = input.value.trim().toLowerCase();
    table.querySelectorAll("tbody tr").forEach((tr) => tr.classList.toggle("filtered-out", q !== "" && !tr.textContent.toLowerCase().includes(q)));
  });
});

// Bulk actions: "select all" checkboxes and the count of selected rows.
document.querySelectorAll("input[data-select-all]").forEach((all) => {
  const table = document.getElementById(all.dataset.selectAll);
  all.addEventListener("change", () => {
    table.querySelectorAll("tbody tr:not(.filtered-out) input[name=sel]").forEach((c) => { c.checked = all.checked; });
    table.dispatchEvent(new Event("selchange"));
  });
});
document.querySelectorAll("form[data-bulk]").forEach((form) => {
  const table = document.getElementById(form.dataset.bulk);
  const label = form.querySelector(".bulk-count");
  const update = () => {
    const n = table.querySelectorAll("input[name=sel]:checked").length;
    label.textContent = n ? n + " selected" : "Select services to act on them";
    form.classList.toggle("has-sel", n > 0);
  };
  table.addEventListener("change", update);
  table.addEventListener("selchange", update);
});

// ---------- activity: what is in progress, from any page of the organization ----------
// The header shows "N in progress" while builds, deployments, databases or backups are
// under way; its menu lists them with their state. When one finishes, a notice says
// so, and the page reloads if it is the page of that item (to show the result).
const activity = document.getElementById("activity");
if (activity) {
  const list = activity.querySelector(".activity-list");
  const count = activity.querySelector(".activity-count");
  const toast = document.getElementById("activity-toast");
  let known = new Map(); // key -> item
  let first = true;
  let toastTimer;
  const here = window.location.pathname;
  const pathOf = (link) => link.split("#")[0];
  const notify = (text, link) => {
    toast.replaceChildren();
    const a = el("a", "", text);
    a.href = link;
    toast.append(a);
    toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { toast.hidden = true; }, 8000);
  };
  const render = (items) => {
    list.replaceChildren();
    for (const it of items) {
      const li = el("li");
      const a = el("a");
      a.href = it.link;
      const head = el("span", "activity-head");
      head.append(el("strong", "", it.label), el("span", "badge st-" + it.state, it.state));
      a.append(head, el("small", "", it.detail));
      li.append(a);
      list.append(li);
    }
    count.textContent = String(items.length);
    activity.hidden = items.length === 0;
  };
  const poll = async () => {
    let delay = 15000;
    try {
      const data = await getJSON(activity.dataset.url);
      const items = data.items || [];
      const next = new Map(items.map((it) => [it.key, it]));
      if (!first) {
        let reload = false;
        for (const [key, it] of known) {
          if (next.has(key)) continue;
          const word = { build: "build finished", deploy: "deployment finished", database: "database ready", backup: it.state === "restoring" ? "restore finished" : "backup finished" }[it.kind] || "finished";
          notify(it.label + ": " + word + ". See the result", it.link);
          if (pathOf(it.link) === here) reload = true;
        }
        if (reload) { setTimeout(() => window.location.reload(), 1200); }
      }
      first = false;
      known = next;
      render(items);
      if (items.length > 0) delay = 3000;
    } catch (_) { /* signed out or offline: try again later */ }
    setTimeout(poll, document.hidden ? Math.max(delay, 15000) : delay);
  };
  poll();
}

// ---------- loading feedback ----------
// The button of a form being submitted shows a spinner and cannot be pressed twice. It is
// disabled after the submit event (a disabled button's name and value would be left out).
document.addEventListener("submit", (e) => {
  if (e.defaultPrevented) return; // a confirmation was declined
  const b = e.submitter || e.target.querySelector("button[type=submit], button:not([type]), input[type=submit]");
  if (!b || e.target.hasAttribute("hx-post") || e.target.target === "_blank") return;
  b.classList.add("is-busy");
  b.setAttribute("aria-busy", "true");
  setTimeout(() => { b.disabled = true; }, 0);
  // A form that only downloads a file does not leave the page: give the button back.
  setTimeout(() => { b.classList.remove("is-busy"); b.removeAttribute("aria-busy"); b.disabled = false; }, 15000);
  // Back/forward cache: the page may come back with the button still busy.
  window.addEventListener("pageshow", () => { b.classList.remove("is-busy"); b.removeAttribute("aria-busy"); b.disabled = false; }, { once: true });
});
