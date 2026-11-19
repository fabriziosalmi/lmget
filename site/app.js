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

// --- the hero's word: models crumbles into weights, then GGUFs, then back,
// once; again when the pointer comes back to it. Letters scramble through
// a few block glyphs, left to right, and settle. Nothing moves with reduced
// motion, and screen readers read the plain word (.sr) ---

const CRUMBLE = "/\\|_-.:";
const STEP = 55; // ms between glyphs
const STAGGER = 45; // ms between letters
const SCRAMBLE = 6; // glyphs a letter goes through
const HOLD = 2200; // ms a word stays

function heroCycle() {
  const slot = document.querySelector(".hero .cycle");
  const live = slot?.querySelector(".cycle-live");
  if (!slot || !live || matchMedia("(prefers-reduced-motion: reduce)").matches) return;
  const words = (slot.dataset.words || "").split(" ").filter(Boolean);
  if (words.length < 2) return;
  const ghosts = words.map((w) => slot.appendChild(el("span", w, "cycle-ghost")));
  const widthOf = (w) => ghosts[words.indexOf(w)].getBoundingClientRect().width;
  const fit = (w) => { slot.style.width = `${widthOf(w)}px`; }; // CSSOM: allowed by the CSP
  fit(words[0]);
  addEventListener("resize", () => fit(live.textContent));
  let running = false;
  let seed = 7;
  const glyph = () => {
    seed = (seed * 1103515245 + 12345) % 2147483648; // the same crumble every time
    return CRUMBLE[seed % CRUMBLE.length];
  };
  const morph = (from, to) => new Promise((done) => {
    fit(to); // the slot slides to the new word's width while it crumbles
    const n = Math.max(from.length, to.length);
    const t0 = performance.now();
    const frame = (now) => {
      const t = now - t0;
      const parts = [];
      let settled = 0;
      for (let i = 0; i < n; i++) {
        const k = Math.floor((t - i * STAGGER) / STEP); // glyphs shown so far at i
        if (k < 0) {
          if (from[i]) parts.push(document.createTextNode(from[i]));
        } else if (k < SCRAMBLE) {
          parts.push(el("span", glyph(), "g"));
        } else {
          if (to[i]) parts.push(document.createTextNode(to[i]));
          settled++;
        }
      }
      live.replaceChildren(...parts);
      if (settled < n) requestAnimationFrame(frame);
      else {
        live.textContent = to;
        done();
      }
    };
    requestAnimationFrame(frame);
  });
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  const run = async () => {
    if (running || document.hidden) return;
    running = true;
    for (let i = 0; i < words.length; i++) {
      await morph(words[i], words[(i + 1) % words.length]);
      if (i < words.length - 1) await wait(HOLD);
    }
    running = false;
  };
  // after the first visit's intro (1.3 s) when it plays, else a moment in
  const start = document.documentElement.classList.contains("intro") ? 2000 : 1200;
  setTimeout(run, start);
  slot.parentElement.addEventListener("pointerenter", run);
}

heroCycle();

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
  box.replaceChildren(...facts);
  box.hidden = false;
}

const [ix, al] = await Promise.all([getJSON("/index.json"), getJSON("/aliases.json")]);
showFacts(validIndex(ix), validAliases(al));
showIndex(validIndex(ix));
showAliases(validAliases(al));
