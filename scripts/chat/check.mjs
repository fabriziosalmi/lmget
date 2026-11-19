#!/usr/bin/env node
// Quality gates of the lmget.org chat, run with the same matcher the site
// uses. Exit 1 on any failure.
//
//   node scripts/chat/check.mjs [--lang en|it] [--lmget PATH]
//
// 1. integrity: unique ids, suggestions that exist, tests on every entry,
//    no em dash or spaced en dash, no emoji, in anything a reader sees;
// 2. battery: every test question answers with its own entry (100%);
// 3. typos: the same questions with one letter changed, transposed, dropped
//    or doubled in one word, deterministic: at least 90% still answer right;
// 4. negatives: questions out of scope get the fallback, never an entry;
// 5. commands: every `lmget ...` in an answer names a real subcommand, and
//    every --flag it uses (and every --flag quoted alone) is in that
//    subcommand's --help of the real binary (--lmget, else go run).

import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..");
const M = createRequire(import.meta.url)(join(root, "site", "chat", "matcher.js"));
const args = process.argv.slice(2);
const opt = (n, d) => (args.indexOf(n) >= 0 ? args[args.indexOf(n) + 1] : d);
const lang = opt("--lang", "en");
const lmgetBin = opt("--lmget", null);
const db = JSON.parse(readFileSync(join(root, "site", "chat", `${lang}.json`), "utf8"));
const all = [...db.entries, ...(db.smalltalk || [])];
const fails = [];
const fail = (gate, msg) => fails.push(`${gate}: ${msg}`);

// 1. integrity
for (const k of ["welcome", "header", "suggestLabel", "resetDone"]) if (typeof db.config[k] !== "string") fail("integrity", `config.${k} missing`);
const ids = new Set();
for (const e of all) {
  if (ids.has(e.id)) fail("integrity", `duplicate id ${e.id}`);
  ids.add(e.id);
  if (!e.tests?.length) fail("integrity", `${e.id} has no tests`);
  if (!e.answers?.length) fail("integrity", `${e.id} has no answers`);
}
for (const e of all) for (const s of e.suggest || []) if (!ids.has(s)) fail("integrity", `${e.id} suggests missing ${s}`);
const visible = [db.config.welcome, db.config.header, db.config.suggestLabel, db.config.resetDone, ...db.fallbacks, ...all.flatMap((e) => [e.question, ...e.answers])];
for (const t of visible) {
  if (t.includes("—")) fail("integrity", `em dash in: ${t.slice(0, 60)}`);
  if (/ – /.test(t)) fail("integrity", `spaced en dash in: ${t.slice(0, 60)}`);
  if (/\p{Extended_Pictographic}/u.test(t)) fail("integrity", `emoji in: ${t.slice(0, 60)}`);
}

// 2. battery
let tests = 0;
for (const e of all) {
  for (const q of e.tests || []) {
    tests++;
    const hit = M.match(q, db);
    if (hit?.id !== e.id) fail("battery", `"${q}" -> ${hit?.id ?? "fallback"}, want ${e.id}`);
  }
}

// 3. typos, deterministic
let seed = 42;
const rnd = (n) => ((seed = (seed * 1103515245 + 12345) % 2147483648) % n);
function mutate(q) {
  const words = q.split(" ");
  const cand = words.map((w, i) => [w, i]).filter(([w]) => /^[\p{L}]{5,}$/u.test(w));
  if (!cand.length) return null;
  const [w, i] = cand[rnd(cand.length)];
  const k = 1 + rnd(w.length - 2);
  const ops = [
    () => w.slice(0, k) + w[k + 1] + w[k] + w.slice(k + 2), // transpose
    () => w.slice(0, k) + w.slice(k + 1), // drop
    () => w.slice(0, k) + w[k] + w.slice(k), // double
    () => w.slice(0, k) + "x" + w.slice(k + 1), // change
  ];
  words[i] = ops[rnd(ops.length)]();
  return words.join(" ");
}
let typoTotal = 0, typoOK = 0;
const typoMiss = [];
for (const e of db.entries) {
  for (const q of e.tests || []) {
    const m = mutate(q);
    if (!m) continue;
    typoTotal++;
    if (M.match(m, db)?.id === e.id) typoOK++;
    else typoMiss.push(`"${m}" (${e.id})`);
  }
}
const typoRate = typoTotal ? typoOK / typoTotal : 1;
if (typoRate < 0.9) fail("typos", `${typoOK}/${typoTotal} (${(typoRate * 100).toFixed(1)}%) < 90%: ${typoMiss.slice(0, 5).join(", ")}`);

// 4. negatives
for (const q of db.negatives || []) {
  const hit = M.match(q, db);
  if (hit) fail("negatives", `"${q}" -> ${hit.id}, want the fallback`);
}

// 5. commands against the real binary
const lmget = (...a) => {
  try {
    return lmgetBin
      ? execFileSync(lmgetBin, a, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] })
      : execFileSync("go", ["run", "./cmd/lmget", ...a], { cwd: root, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
  } catch (e) {
    return (e.stdout || "") + (e.stderr || "");
  }
};
const rootHelp = lmget("--help");
const subs = new Set([...rootHelp.matchAll(/^\s{2}([a-z][a-z-]*)\s{2,}/gm)].map((m) => m[1]));
const helpOf = new Map();
const help = (sub) => {
  if (!helpOf.has(sub)) helpOf.set(sub, sub ? lmget(sub, "--help") : lmget("get", "--help") + rootHelp);
  return helpOf.get(sub);
};
let cmds = 0;
for (const e of db.entries) {
  for (const a of e.answers) {
    const snippets = [...a.matchAll(/```[a-z]*\n([\s\S]*?)```/g)].flatMap((m) => m[1].split("\n"))
      .concat([...a.replace(/```[a-z]*\n[\s\S]*?```/g, " ").matchAll(/`([^`\n]+)`/g)].map((m) => m[1]));
    for (const s of snippets) {
      const at = s.search(/(^|\s)lmget(\s|$)/);
      let toks = (at >= 0 ? s.slice(at).trim() : s.trim()).split(/\s+/);
      // `claude mcp add lmget -- lmget mcp`: the command lmget runs is after the
      // end-of-options marker, and the marker itself is no flag
      const dd = toks.indexOf("--");
      if (dd >= 0 && toks[dd + 1] === "lmget") toks = toks.slice(dd + 1);
      if (toks[0] === "lmget") {
        cmds++;
        const sub = subs.has(toks[1]) ? toks[1] : "";
        for (const f of toks.filter((t) => t.startsWith("--") && t !== "--")) {
          const flag = f.split("=")[0];
          if (!new RegExp(`(^|\\s)${flag}(\\s|=|,|$)`, "m").test(help(sub))) fail("commands", `${e.id}: "${s}": ${flag} not in lmget ${sub || "get"} --help`);
        }
      } else if (/^--[a-z][a-z-]*/.test(toks[0])) {
        cmds++;
        const flag = toks[0].split("=")[0];
        if (![...subs, ""].some((sub) => new RegExp(`(^|\\s)${flag}(\\s|=|,|$)`, "m").test(help(sub)))) fail("commands", `${e.id}: ${flag} is no flag of lmget`);
      }
    }
  }
}

console.log(`entries ${db.entries.length} + smalltalk ${(db.smalltalk || []).length}; tests ${tests}; typos ${typoOK}/${typoTotal} (${(typoRate * 100).toFixed(1)}%); negatives ${(db.negatives || []).length}; commands checked ${cmds}; subcommands known ${subs.size}`);
if (fails.length) {
  console.log(`FAIL (${fails.length})`);
  for (const f of fails) console.log("  " + f);
  process.exit(1);
}
console.log("OK");
