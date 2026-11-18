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

function validIndex(ix) {
  return ix && Array.isArray(ix.models) ? ix : null;
}

function validAliases(al) {
  return al && typeof al.aliases === "object" && al.aliases !== null ? al : null;
}

function showIndex(ix) {
  const panel = document.getElementById("index-panel");
  if (!panel || !ix) return;

  const models = ix.models.map((m) => ({
    name: String(m.ref || "").replace(/^hf:/, ""),
    license: String(m.license || ""),
    files: (m.files || []).length,
    bytes: (m.files || []).reduce((s, f) => s + (f.size || 0), 0),
  })).sort((a, b) => b.bytes - a.bytes);
  // one segment per model, as wide as its share of the bytes
  const strip = el("div", undefined, "strip");
  strip.setAttribute("aria-hidden", "true");
  const table = el("table");
  const head = el("tr");
  for (const [h, c] of [["Model", ""], ["License", ""], ["Files", "num"], ["Size", "num"]]) head.append(el("th", h, c));
  table.append(el("thead"), el("tbody"));
  table.tHead.append(head);
  models.forEach((m, i) => {
    const seg = el("span");
    seg.style.flexGrow = String(m.bytes);
    seg.title = `${m.name}: ${bytes(m.bytes)}`;
    strip.append(seg);
    const row = el("tr");
    const name = el("td", undefined, "model");
    const slash = m.name.indexOf("/");
    if (slash > 0) name.append(el("span", m.name.slice(0, slash + 1), "org"));
    name.append(el("span", m.name.slice(slash + 1), "name"));
    row.append(name, el("td", m.license, "lic"), el("td", m.files === 1 ? "1 file" : m.files + " files", "num files"), el("td", bytes(m.bytes), "num size"));
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
  const scroll = el("div", undefined, "table models");
  scroll.append(table);
  panel.replaceChildren(strip, scroll, note);
}

function showAliases(al) {
  const panel = document.getElementById("aliases-panel");
  if (!panel || !al) return;
  const table = el("table");
  const head = el("tr");
  for (const [h, c] of [["Nickname", ""], ["Family", ""], ["Formats", ""], ["Sizes", "num"]]) head.append(el("th", h, c));
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
    const family = el("td", undefined, "family");
    family.append(el("span", String(a.family || ""), "fam"));
    if (a.kind && a.kind !== "chat") family.append(" ", el("span", a.kind, "kind"));
    row.append(el("td", name, "key"), family, el("td", formats, "fmt"), el("td", range, "num size"));
    table.tBodies[0].append(row);
  }
  const note = el("p", undefined, "note");
  note.append(
    `${Object.keys(al.aliases).length} nicknames, signed like the index, valid until `,
    el("span", date(al.expires), "nowrap"),
    ".",
  );
  const scroll = el("div", undefined, "table nick");
  scroll.append(table);
  panel.replaceChildren(scroll, note);
}

// --- the facts under the install command, read from the signed files ---

function showFacts(ix, al) {
  const box = document.getElementById("facts");
  if (!box || !ix) return; // without the index the hero stays as it is
  const total = ix.models.reduce((s, m) => s + (m.files || []).reduce((t, f) => t + (f.size || 0), 0), 0);
  const left = Math.floor((new Date(ix.expires) - Date.now()) / DAY);
  const facts = [
    stat(String(ix.models.length), "models mirrored"),
    stat(bytes(total), "verified by sha256"),
    stat(String((ix.nodes || []).length), "mirror nodes"),
  ];
  if (al) facts.push(stat(String(Object.keys(al.aliases).length), "signed nicknames"));
  facts.push(stat(
    Number.isFinite(left) ? (left >= 0 ? (left === 1 ? "1 day" : left + " days") : "expired") : "?",
    "until the signature expires",
    left < 7 ? "warn" : "",
  ));
  const src = el("p", undefined, "facts-source");
  src.append("Read now from the signed ");
  const a1 = el("a", "index"); a1.href = "/index.json";
  src.append(a1, ` (sequence ${ix.sequence})`);
  if (al) {
    const a2 = el("a", "nickname list"); a2.href = "/aliases.json";
    src.append(" and ", a2, ` (sequence ${al.sequence})`);
  }
  src.append(".");
  box.replaceChildren(...facts, src);
  box.hidden = false;
}

const [ix, al] = await Promise.all([getJSON("/index.json"), getJSON("/aliases.json")]);
showFacts(validIndex(ix), validAliases(al));
showIndex(validIndex(ix));
showAliases(validAliases(al));
