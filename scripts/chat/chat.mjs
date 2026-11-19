#!/usr/bin/env node
// The lmget.org chat in a terminal: the same pipeline (site/chat/prep.js,
// matcher.js, spell.js, session.js)
// and the same answers (site/chat/<lang>.json) the site will use, so what is
// tried here is what ships. No model: every answer is written and checked.
//
//   node scripts/chat/chat.mjs [--lang en|it] [--fast] [--debug]
//   node scripts/chat/chat.mjs --once "how do I install lmget?"
//   node scripts/chat/chat.mjs --lang it --once "come installo lmget?"
//
// In the chat: /debug shows which entry answered and its score, /reset
// forgets the thread, /exit (the language's own word too, see config
// exitCommands; or Ctrl-D) leaves. The few words of the interface come
// from the config of <lang>.json, like the answers.

import readline from "node:readline";
import { loadChat } from "./load.mjs";

const args = process.argv.slice(2);
const opt = (name, def) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : def;
};
const lang = opt("--lang", "en");
const once = opt("--once", null);
const fast = args.includes("--fast") || !process.stdout.isTTY;
const chat = loadChat(lang);
const db = chat.db;
const ui = db.config;
const exits = new Set(["/exit", ...(ui.exitCommands || [])]);

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

// --- one turn: site/chat/session.js, as on the site ---
const talk = chat.session();
let debug = args.includes("--debug");

async function turn(text) {
  const a = talk.answer(text);
  if (debug) {
    const r = a.result;
    process.stdout.write(D(`[${a.id ?? "fallback"} score ${r.score != null ? r.score.toFixed(2) : "-"}${r.coverage != null ? ` coverage ${r.coverage.toFixed(2)}` : ""} · prepped: "${r.prepped}"${r.corrected ? ` · corrected: "${r.corrected}"` : ""}]`) + "\n");
  }
  await say(render(a.text));
  if (a.suggest.length) process.stdout.write(D("\n" + ui.suggestLabel + a.suggest.map((x) => x.question).join(" | ")) + "\n");
}

if (once) {
  await turn(once);
  process.exit(0);
}

process.stdout.write(D(ui.header) + "\n\n");
await say(render(ui.welcome));
const rl = readline.createInterface({ input: process.stdin, output: process.stdout, prompt: "\n› " });
rl.prompt();
for await (const line of rl) {
  const text = line.trim();
  if (exits.has(text)) break;
  if (text === "/debug") {
    debug = !debug;
    process.stdout.write(D(`debug ${debug ? "on" : "off"}`) + "\n");
  } else if (text === "/reset") {
    talk.reset();
    process.stdout.write(D(ui.resetDone) + "\n");
  } else if (text) {
    process.stdout.write("\n");
    await turn(text);
  }
  rl.prompt();
}
rl.close();
