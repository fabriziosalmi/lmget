// lmget.org: copy buttons and the mirrored models panel. The page works
// without this file; it only adds to it.

// --- copy buttons --------------------------------------------------------

const timers = new WeakMap();

function report(button, ok) {
  const tip = button.nextElementSibling; // the role="status" bubble
  clearTimeout(timers.get(button));
  button.removeAttribute("data-copied");
  button.removeAttribute("data-failed");
  tip.textContent = "";
  // a new text node each time, so a repeated message is announced again
  requestAnimationFrame(() => {
    button.setAttribute(ok ? "data-copied" : "data-failed", "");
    tip.textContent = ok ? "Copied" : "Copy failed";
  });
  timers.set(button, setTimeout(() => {
    button.removeAttribute("data-copied");
    button.removeAttribute("data-failed");
    tip.textContent = "";
  }, 2000));
}

async function copy(button, target) {
  const text = target.innerText.trim();
  if (!text) return;
  try {
    await navigator.clipboard.writeText(text);
    report(button, true);
  } catch {
    report(button, false); // clipboard access can be denied
  }
}

if ("CommandEvent" in window) {
  // command events do not bubble: listen in the capture phase
  document.addEventListener("command", (event) => {
    if (event.command === "--copy") copy(event.source, event.target);
  }, true);
} else {
  document.addEventListener("click", (event) => {
    const button = event.target.closest('button[command="--copy"]');
    if (!button) return;
    const target = document.getElementById(button.getAttribute("commandfor"));
    if (target) copy(button, target);
  });
}

for (const wrap of document.querySelectorAll(".copy[hidden]")) {
  wrap.hidden = false; // the buttons work now: show them
}

// --- mirrored models -----------------------------------------------------

function el(tag, text, cls) {
  const e = document.createElement(tag);
  if (text !== undefined) e.textContent = text; // never innerHTML: the index is data
  if (cls) e.className = cls;
  return e;
}

function bytes(n) {
  const units = ["B", "kB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1000 && i < units.length - 1) {
    n /= 1000;
    i++;
  }
  return `${n >= 100 || i === 0 ? n.toFixed(0) : n.toFixed(1)} ${units[i]}`;
}

function date(s) {
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? "" : d.toISOString().slice(0, 10);
}

async function showIndex() {
  const panel = document.getElementById("index-panel");
  if (!panel) return;
  let ix;
  try {
    const resp = await fetch("/index.json", { cache: "no-cache" });
    if (!resp.ok) return;
    ix = await resp.json();
  } catch {
    return; // keep the static text
  }
  if (!Array.isArray(ix.models)) return;

  const total = ix.models.reduce((sum, m) => sum + (m.files || []).reduce((s, f) => s + (f.size || 0), 0), 0);
  const summary = el("p", undefined, "note");
  summary.append(
    `Index sequence ${ix.sequence}: ${ix.models.length} models, ${bytes(total)}, served by ${(ix.nodes || []).length} nodes, valid until `,
    el("span", date(ix.expires), "nowrap"),
    ". The CLI verifies its signature; this page only shows it.",
  );

  const table = el("table");
  const head = el("tr");
  for (const h of ["Model", "License", "Files", "Size"]) head.append(el("th", h));
  table.append(el("thead"), el("tbody"));
  table.tHead.append(head);
  for (const m of ix.models) {
    const files = m.files || [];
    const row = el("tr");
    row.append(
      el("td", String(m.ref || "").replace(/^hf:/, "")),
      el("td", String(m.license || "")),
      el("td", String(files.length), "num"),
      el("td", bytes(files.reduce((s, f) => s + (f.size || 0), 0)), "num"),
    );
    table.tBodies[0].append(row);
  }
  const scroll = el("div", undefined, "table");
  scroll.append(table);
  panel.replaceChildren(summary, scroll);
}

showIndex();
