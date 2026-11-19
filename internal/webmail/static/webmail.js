// Webmail helpers (no inline scripts under the CSP): select every message, confirm
// destructive buttons, close the account menu on an outside click.
document.addEventListener("change", (e) => {
  const all = e.target.closest("[data-select-all]");
  if (!all) return;
  document.querySelectorAll('input[name="sel"]').forEach((c) => { c.checked = all.checked; });
});
document.addEventListener("click", (e) => {
  const b = e.target.closest("[data-confirm]");
  if (b && !window.confirm(b.dataset.confirm)) e.preventDefault();
  document.querySelectorAll("details.menu[open]").forEach((d) => { if (!d.contains(e.target)) d.removeAttribute("open"); });
});
document.addEventListener("keydown", (e) => {
  if (e.key === "/" && !/^(INPUT|TEXTAREA)$/.test(document.activeElement?.tagName || "")) {
    const q = document.querySelector(".search input");
    if (q) { e.preventDefault(); q.focus(); }
  }
  if (e.key === "Escape") document.querySelectorAll("details.menu[open]").forEach((d) => d.removeAttribute("open"));
});
// The message frame takes the height of the message.
const frame = document.querySelector(".body-frame");
if (frame) frame.addEventListener("load", () => {
  try { frame.style.height = Math.max(240, frame.contentDocument.documentElement.scrollHeight + 24) + "px"; } catch (_) { /* sandboxed: keep the default height */ }
});
