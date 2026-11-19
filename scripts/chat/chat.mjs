#!/usr/bin/env node
// The lmget.org chat in a terminal: the same matcher (site/chat/matcher.js)
// and the same answers (site/chat/<lang>.json) the site will use, so what is
// tried here is what ships. No model: every answer is written and checked.
//
//   node scripts/chat/chat.mjs [--lang it] [--fast]
//   node scripts/chat/chat.mjs --lang it --once "come installo lmget?"
//
// In the chat: /debug shows which entry answered and its score, /reset
// forgets the thread, /esci (or /exit, Ctrl-D) leaves.

import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import readline from "node:readline";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..");
const M = createRequire(import.meta.url)(join(root, "site", "chat", "matcher.js"));

const args = process.argv.slice(2);
const opt = (name, def) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : def;
};
const lang = opt("--lang", "it");
const once = opt("--once", null);
const fast = args.includes("--fast") || !process.stdout.isTTY;
const db = JSON.parse(readFileSync(join(root, "site", "chat", `${lang}.json`), "utf8"));
const byId = new Map([...db.entries, ...(db.smalltalk || [])].map((e) => [e.id, e]));

// --- terminal rendering of the answers' small markdown: black and white ---
const tty = process.stdout.isTTY;
const B = (s) => (tty ? `\x1b[1m${s}\x1b[22m` : s);
const D = (s) => (tty ? `\x1b[2m${s}\x1b[22m` : s);

function render(md) {
  const out = [];
  let fence = false;
  for (const line of md.split("\n")) {
    if (line.startsWith("```")) {
      fence = !fence;
      continue;
    }
    if (fence) {
      out.push("    " + B(line));
      continue;
    }
    out.push(
      line
        .replace(/\*\*([^*]+)\*\*/g, (_, t) => B(t))
        .replace(/`([^`]+)`/g, (_, t) => B(t))
        .replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_, t, u) => `${t} ${D("<" + u + ">")}`),
    );
  }
  return out.join("\n");
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function say(text) {
  if (fast) {
    process.stdout.write(text + "\n");
    return;
  }
  for (const part of text.split(/(\s+)/)) {
    process.stdout.write(part);
    if (part.trim()) await sleep(12);
  }
  process.stdout.write("\n");
}

// --- one turn ---
const cursor = Object.create(null); // answer variant per entry
let ctx = null; // the thread's entry, for short follow-ups
let fallbackAt = 0;
let debug = false;

function answer(text) {
  const hit = M.match(text, db, { ctxEntry: ctx });
  if (!hit) {
    const f = db.fallbacks[fallbackAt++ % db.fallbacks.length];
    return { id: null, text: f, suggest: [] };
  }
  const n = cursor[hit.id] || 0;
  cursor[hit.id] = n + 1;
  if (!hit.id.startsWith("st-")) ctx = hit;
  return { id: hit.id, text: hit.answers[n % hit.answers.length], suggest: hit.suggest || [] };
}

function scoreOf(text, id) {
  const e = byId.get(id);
  return e ? M.scoreEntry(e, M.norm(text), M.tokens(text)).toFixed(2) : "-";
}

async function turn(text) {
  const a = answer(text);
  if (debug) process.stdout.write(D(`[${a.id ?? "fallback"} score ${a.id ? scoreOf(text, a.id) : "-"}]`) + "\n");
  await say(render(a.text));
  const next = a.suggest.map((id) => byId.get(id)?.question).filter(Boolean);
  if (next.length) process.stdout.write(D("\nPuoi chiedermi: " + next.join(" | ")) + "\n");
}

if (once) {
  await turn(once);
  process.exit(0);
}

process.stdout.write(D(`lmget chat (${lang}) · nessun modello: risposte scritte e verificate · /debug /reset /esci`) + "\n\n");
await say(render(db.config.welcome));
const rl = readline.createInterface({ input: process.stdin, output: process.stdout, prompt: "\n› " });
rl.prompt();
for await (const line of rl) {
  const text = line.trim();
  if (text === "/esci" || text === "/exit") break;
  if (text === "/debug") {
    debug = !debug;
    process.stdout.write(D(`debug ${debug ? "on" : "off"}`) + "\n");
  } else if (text === "/reset") {
    ctx = null;
    process.stdout.write(D("thread dimenticato") + "\n");
  } else if (text) {
    process.stdout.write("\n");
    await turn(text);
  }
  rl.prompt();
}
rl.close();
