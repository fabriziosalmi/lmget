// lmget.org's chat: the answers of /chat/<lang>.json through the pipeline the
// terminal chat and the gates use (prep.js, matcher.js, spell.js,
// session.js). No model and no server: it runs here, on files of this site.
// app.js imports this module the first time the form is touched. DOM APIs
// only: an answer is data, never HTML.

// a link goes to https, or to this site
const SAFE_URL = /^(https:\/\/|\/(?!\/)|#)/;
// what lmget.org shows of an answer: `code`, **bold**, [text](url), bare https URLs
const INLINE = /`([^`\n]+)`|\*\*([^*\n]+)\*\*|\[([^\]\n]+)\]\(([^)\s]+)\)|(https:\/\/[^\s<>()]*[^\s<>().,;:!?'"])/g;
const KEEP = 20; // turns kept on screen

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function link(text, href) {
  if (!SAFE_URL.test(href)) return document.createTextNode(text);
  const a = el("a", null, text);
  a.href = href;
  return a;
}

function inline(parent, text) {
  let at = 0;
  for (const m of text.matchAll(INLINE)) {
    if (m.index > at) parent.append(text.slice(at, m.index));
    if (m[1] !== undefined) parent.append(el("code", null, m[1]));
    else if (m[2] !== undefined) parent.append(el("strong", null, m[2]));
    else if (m[3] !== undefined) parent.append(link(m[3], m[4]));
    else parent.append(link(m[5], m[5]));
    at = m.index + m[0].length;
  }
  if (at < text.length) parent.append(text.slice(at));
}

// the answers' markdown: fenced code, "- " lists, paragraphs, and inline
export function render(md) {
  const out = document.createDocumentFragment();
  const lines = md.split("\n");
  const list = (l) => l.startsWith("- ");
  const fence = (l) => l.startsWith("```");
  for (let i = 0; i < lines.length; ) {
    if (fence(lines[i])) {
      const code = [];
      for (i++; i < lines.length && !fence(lines[i]); i++) code.push(lines[i]);
      i++;
      const pre = el("pre");
      pre.tabIndex = 0; // a long line scrolls: reachable by keyboard
      pre.append(el("code", null, code.join("\n")));
      out.append(pre);
    } else if (list(lines[i])) {
      const ul = el("ul");
      for (; i < lines.length && list(lines[i]); i++) {
        const li = el("li");
        inline(li, lines[i].slice(2));
        ul.append(li);
      }
      out.append(ul);
    } else if (!lines[i].trim()) {
      i++;
    } else {
      const text = [];
      for (; i < lines.length && lines[i].trim() && !fence(lines[i]) && !list(lines[i]); i++) text.push(lines[i]);
      const p = el("p");
      inline(p, text.join(" "));
      out.append(p);
    }
  }
  return out;
}

// v: the version query of this deploy ("?v=<hash>", or "")
export async function start(form, v) {
  // classic scripts that set self.Zero*: in this order, prep.js before spell.js
  for (const f of ["prep", "matcher", "spell", "session"]) await import(`/chat/${f}.js${v}`);
  const { ZeroMatcher: M, ZeroSpell: Spell, ZeroSession: S } = self;
  const get = async (path) => {
    const resp = await fetch(path + v);
    if (!resp.ok) throw new Error(`${path}: HTTP ${resp.status}`);
    return resp.json();
  };
  const input = form.querySelector(".ask-row input");
  const label = form.querySelector(".ask-row label");
  const button = form.querySelector(".ask-row button");
  const note = form.querySelector(".ask-note-text");
  const other = form.querySelector(".ask-lang");
  const log = form.querySelector(".ask-log");

  const code = await get("/chat/code.json");
  const langs = {};
  let now = null;
  async function use(lang) {
    langs[lang] ??= get(`/chat/${lang}.json`).then((db) => ({ lang, db, talk: S.session(db, code, M, Spell) }));
    now = await langs[lang];
    const c = now.db.config;
    form.lang = lang;
    label.textContent = c.siteLabel;
    input.placeholder = c.sitePlaceholder;
    button.textContent = c.siteAsk;
    note.textContent = c.siteNote;
    other.textContent = c.siteOtherLabel;
    other.lang = c.siteOther;
  }
  await use("en");
  other.addEventListener("click", () => use(other.lang));

  function ask(text) {
    const q = text.trim().slice(0, 500);
    if (!q) return;
    const { db, talk, lang } = now;
    const a = talk.answer(q);
    const turn = el("article", "ask-turn");
    turn.lang = lang;
    const asked = el("p", "ask-q");
    asked.append(el("span", "sr", db.config.siteYou + ": "), q);
    const said = el("div", "ask-a");
    said.append(render(a.text));
    turn.append(asked, said);
    if (a.suggest.length) {
      const next = el("p", "ask-next");
      next.append(el("span", null, db.config.suggestLabel.trim()));
      for (const s of a.suggest) {
        const b = el("button", "ask-chip", s.question);
        b.type = "button";
        b.addEventListener("click", () => ask(s.question));
        next.append(" ", b);
      }
      turn.append(next);
    }
    log.append(turn);
    while (log.children.length > KEEP) log.firstElementChild.remove();
    log.hidden = false;
    log.scrollTop = turn.offsetTop; // the new question at the top of the log (positioned: see site.css)
  }

  return {
    submit() {
      ask(input.value);
      input.value = "";
    },
  };
}
