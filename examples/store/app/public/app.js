const $ = (s) => document.querySelector(s);
const cart = new Map(); // product id -> { product, qty }
const money = (c) => (c / 100).toFixed(2) + " €";

async function api(path, opts = {}) {
  const r = await fetch("/api" + path, { headers: { "Content-Type": "application/json" }, ...opts });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.error || `HTTP ${r.status}`);
  return body;
}

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  for (const c of children) e.append(c);
  return e;
}

async function load() {
  const list = $("#products");
  try {
    const products = await api("/products");
    list.replaceChildren(...products.map((p) => {
      const add = el("button", { type: "button" }, p.stock > 0 ? "Add to cart" : "Sold out");
      add.disabled = p.stock <= 0;
      add.addEventListener("click", () => {
        const line = cart.get(p.id) || { product: p, qty: 0 };
        line.qty++;
        cart.set(p.id, line);
        render();
      });
      const card = el("article", { class: "product" },
        p.image ? el("img", { src: p.image, alt: p.name }) : el("div", { class: "placeholder", "aria-hidden": "true" }, p.name[0]),
        el("h2", {}, p.name), el("p", {}, p.description),
        el("p", { class: "price" }, money(p.price_cents), el("span", { class: "muted" }, ` · ${p.stock} in stock`)), add);
      return card;
    }));
  } catch (e) {
    list.replaceChildren(el("p", { class: "error" }, "The catalogue could not be loaded: " + e.message));
  }
}

function render() {
  let n = 0, total = 0;
  const lines = [];
  for (const { product, qty } of cart.values()) {
    n += qty;
    total += qty * product.price_cents;
    lines.push(el("li", {}, `${qty} × ${product.name} — ${money(qty * product.price_cents)}`));
  }
  $("#cart-count").textContent = n;
  $("#cart-lines").replaceChildren(...lines);
  $("#cart-total").textContent = n ? "Total: " + money(total) : "Your cart is empty.";
}

$("#cart-button").addEventListener("click", () => { render(); $("#cart").showModal(); });
$("#close-cart").addEventListener("click", () => $("#cart").close());
$("#checkout").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const email = new FormData(ev.target).get("email");
  const items = [...cart.values()].map(({ product, qty }) => ({ id: product.id, qty }));
  $("#checkout-result").textContent = "Placing your order…";
  try {
    const o = await api("/orders", { method: "POST", body: JSON.stringify({ email, items }) });
    cart.clear();
    render();
    $("#checkout-result").textContent = `Order #${o.id} placed (${money(o.total_cents)}). A confirmation email is on its way.`;
    load();
  } catch (e) {
    $("#checkout-result").textContent = "The order failed: " + e.message;
  }
});
load();
