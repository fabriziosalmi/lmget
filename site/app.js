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
  // 999.97 MB would print as "1000 MB": step up once it rounds to 1000
  while ((n >= 1000 || (i > 0 && n.toFixed(n >= 100 ? 0 : 1) === "1000")) && i < units.length - 1) {
    n /= 1000;
    i++;
  }
  return `${n >= 100 || i === 0 ? n.toFixed(0) : n.toFixed(1)} ${units[i]}`;
}

function date(s) {
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? "" : d.toISOString().slice(0, 10);
}

async function getJSON(path) {
  try {
    const resp = await fetch(path, { cache: "no-cache" });
    return resp.ok ? await resp.json() : null;
  } catch {
    return null; // the static text stays
  }
}

const DAY = 86400000;

function stat(value, label, cls) {
  const d = el("div", undefined, "stat" + (cls ? " " + cls : ""));
  d.append(el("span", value, "stat-value"), el("span", label, "stat-label"));
  return d;
}

async function showIndex() {
  const panel = document.getElementById("index-panel");
  if (!panel) return;
  const ix = await getJSON("/index.json");
  if (!ix || !Array.isArray(ix.models)) return;

  const models = ix.models.map((m) => ({
    name: String(m.ref || "").replace(/^hf:/, ""),
    license: String(m.license || ""),
    files: (m.files || []).length,
    bytes: (m.files || []).reduce((s, f) => s + (f.size || 0), 0),
  })).sort((a, b) => b.bytes - a.bytes);
  const total = models.reduce((s, m) => s + m.bytes, 0);
  const files = models.reduce((s, m) => s + m.files, 0);
  const left = Math.floor((new Date(ix.expires) - Date.now()) / DAY);

  const stats = el("div", undefined, "stats");
  stats.append(
    stat(String(models.length), "models"),
    stat(String(files), "files"),
    stat(bytes(total), "verified bytes"),
    stat(String((ix.nodes || []).length), "nodes"),
    stat(Number.isFinite(left) ? (left >= 0 ? left + " d" : "expired") : "?", "until expiry", left < 7 ? "warn" : ""),
  );

  // one segment per model, as wide as its share of the bytes
  const strip = el("div", undefined, "strip");
  strip.setAttribute("aria-hidden", "true");
  const table = el("table");
  const head = el("tr");
  for (const h of ["Model", "License", "Files", "Size"]) head.append(el("th", h));
  table.append(el("thead"), el("tbody"));
  table.tHead.append(head);
  models.forEach((m, i) => {
    const seg = el("span");
    seg.style.flexGrow = String(m.bytes);
    seg.title = `${m.name}: ${bytes(m.bytes)}`;
    strip.append(seg);
    const row = el("tr");
    row.append(el("td", m.name), el("td", m.license), el("td", String(m.files), "num"), el("td", bytes(m.bytes), "num"));
    const on = (v) => () => { seg.toggleAttribute("data-on", v); row.toggleAttribute("data-on", v); };
    for (const x of [seg, row]) {
      x.addEventListener("pointerenter", on(true));
      x.addEventListener("pointerleave", on(false));
    }
    table.tBodies[0].append(row);
  });

  const note = el("p", undefined, "note");
  note.append(
    `Read from the signed index, sequence ${ix.sequence}, valid until `,
    el("span", date(ix.expires), "nowrap"),
    ". lmget verifies its signature before using it; this page only shows it.",
  );
  const scroll = el("div", undefined, "table");
  scroll.append(table);
  panel.replaceChildren(stats, strip, scroll, note);
}

async function showAliases() {
  const panel = document.getElementById("aliases-panel");
  if (!panel) return;
  const al = await getJSON("/aliases.json");
  if (!al || typeof al.aliases !== "object" || al.aliases === null) return;
  const table = el("table");
  const head = el("tr");
  for (const h of ["Nickname", "Family", "Formats", "Sizes"]) head.append(el("th", h));
  table.append(el("thead"), el("tbody"));
  table.tHead.append(head);
  for (const name of Object.keys(al.aliases).sort()) {
    const a = al.aliases[name] || {};
    const vs = Array.isArray(a.variants) ? a.variants : [];
    const sizes = vs.map((v) => v.bytes || 0).filter((n) => n > 0);
    const formats = [...new Set(vs.map((v) => String(v.format || "")))].join(", ");
    const lo = bytes(Math.min(...sizes)), hi = bytes(Math.max(...sizes));
    const range = !sizes.length ? "" : lo === hi ? hi : `${lo} to ${hi}`;
    const row = el("tr");
    const family = el("td", String(a.family || ""));
    if (a.kind && a.kind !== "chat") family.append(el("span", ` (${a.kind})`, "dimmed"));
    row.append(el("td", name, "mono"), family, el("td", formats), el("td", range, "num"));
    table.tBodies[0].append(row);
  }
  const note = el("p", undefined, "note");
  note.append(
    `${Object.keys(al.aliases).length} nicknames, signed like the index, valid until `,
    el("span", date(al.expires), "nowrap"),
    ". lmget picks the variant that fits the machine and asks before downloading.",
  );
  const scroll = el("div", undefined, "table wrap-text");
  scroll.append(table);
  panel.replaceChildren(scroll, note);
}

showIndex();
showAliases();
