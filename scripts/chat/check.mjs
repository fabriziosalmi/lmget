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
// 4b. prep: every normalisation rule is needed by its own example;
// 7. ids: scripts/chat/ids-<lang>.tsv matches the json (ids.mjs writes it);
// 8. code: site/chat/code.json matches the tracked files (code.mjs writes it);
// 9. rules: every rule of ask() is needed by an example (see below).
// 5. commands: every `lmget ...` in an answer names a real subcommand, and
//    every --flag it uses (and every --flag quoted alone) is in that
//    subcommand's --help of the real binary (--lmget, else go run).

import { readFileSync, readdirSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { idsTsv, idsPath } from "./ids.mjs";
import { M, Spell, Prep, loadChat } from "./load.mjs";
import { codeJson, codePath } from "./code.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..");
const args = process.argv.slice(2);
const opt = (n, d) => (args.indexOf(n) >= 0 ? args[args.indexOf(n) + 1] : d);
const lang = opt("--lang", "en");
const lmgetBin = opt("--lmget", null);
const chat = loadChat(lang);
const db = chat.db;
const all = [...db.entries, ...(db.smalltalk || [])];
const ask = (q) => chat.ask(q).hit; // what the chat answers: code index, matcher, spelling repair
const fails = [];
const fail = (gate, msg) => fails.push(`${gate}: ${msg}`);

// 1. integrity
for (const k of ["welcome", "header", "suggestLabel", "resetDone", "codeAnswer", "closestLabel"]) if (typeof db.config[k] !== "string") fail("integrity", `config.${k} missing`);
const ids = new Set();
for (const e of all) {
  if (ids.has(e.id)) fail("integrity", `duplicate id ${e.id}`);
  ids.add(e.id);
  if (!e.tests?.length) fail("integrity", `${e.id} has no tests`);
  if (!e.answers?.length) fail("integrity", `${e.id} has no answers`);
}
for (const e of all) for (const s of e.suggest || []) if (!ids.has(s)) fail("integrity", `${e.id} suggests missing ${s}`);
const visible = [db.config.welcome, db.config.header, db.config.suggestLabel, db.config.resetDone, db.config.codeAnswer, db.config.closestLabel, ...db.fallbacks, ...all.flatMap((e) => [e.question, ...e.answers])];
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
    const hit = ask(q); // what the chat answers
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
    if (ask(m)?.id === e.id) typoOK++;
    else typoMiss.push(`"${m}" (${e.id})`);
  }
}
const typoRate = typoTotal ? typoOK / typoTotal : 1;
if (typoRate < 0.9) fail("typos", `${typoOK}/${typoTotal} (${(typoRate * 100).toFixed(1)}%) < 90%: ${typoMiss.slice(0, 5).join(", ")}`);

// 3b. heavy typos: two edits in one word of 7+ letters; answered by the
// spelling repair when the matcher alone misses
function mutate2(q) {
  const words = q.split(" ");
  const cand = words.map((w, i) => [w, i]).filter(([w]) => /^[\p{L}]{7,}$/u.test(w));
  if (!cand.length) return null;
  const [w0, i] = cand[rnd(cand.length)];
  let w = w0;
  for (let n = 0; n < 2; n++) {
    const k = 1 + rnd(w.length - 3);
    w = n === 0 ? w.slice(0, k) + w[k + 1] + w[k] + w.slice(k + 2) : w.slice(0, k) + w.slice(k + 1);
  }
  words[i] = w;
  return words.join(" ");
}
let heavyTotal = 0, heavyOK = 0;
for (const e of db.entries) {
  for (const q of e.tests || []) {
    const m = mutate2(q);
    if (!m) continue;
    heavyTotal++;
    if (ask(m)?.id === e.id) heavyOK++;
  }
}
const heavyRate = heavyTotal ? heavyOK / heavyTotal : 1;
if (heavyRate < 0.6) fail("heavy typos", `${heavyOK}/${heavyTotal} (${(heavyRate * 100).toFixed(1)}%) < 60%`);

// 4. negatives
for (const q of db.negatives || []) {
  const hit = ask(q);
  if (hit) fail("negatives", `"${q}" -> ${hit.id}, want the fallback`);
}

// 4b. normalisation: every prep.js rule of this language rewrites its own
// example, which then answers with its entry; with that rule off, the
// example does not: a rule no example needs is a rule to remove
let prepRules = 0;
for (const [name, , , ex, want] of Prep.RULES[lang] || []) {
  prepRules++;
  const off = new Set([name]);
  if (Prep.prep(ex, lang) === Prep.prep(ex, lang, { off })) fail("prep", `${name}: does not rewrite its example "${ex}"`);
  const on = ask(ex)?.id ?? "fallback";
  const without = chat.ask(ex, { prepOff: off }).hit?.id ?? "fallback";
  if (on !== want) fail("prep", `${name}: "${ex}" -> ${on}, want ${want}`);
  if (without === want) fail("prep", `${name}: "${ex}" answers ${want} without the rule too`);
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

// 6. coverage: every subcommand of lmget, every flag in its --help and every
// environment variable the code reads is named in at least one answer, so a
// new flag or variable fails the gate until the chat explains it
const answersText = db.entries.flatMap((e) => e.answers).join("\n");
const covSubs = [...subs].filter((s) => s !== "help" && s !== "completion");
const covFlags = new Set();
for (const sub of ["", ...covSubs]) {
  for (const m of help(sub).matchAll(/^\s+(?:-\w, )?(--[a-z][a-z0-9-]*)/gm)) if (m[1] !== "--help") covFlags.add(m[1]);
}
const covEnv = new Set();
for (const dir of [root, join(root, "cmd", "lmget")]) {
  for (const f of readdirSync(dir)) {
    if (!f.endsWith(".go") || f.endsWith("_test.go")) continue;
    for (const m of readFileSync(join(dir, f), "utf8").matchAll(/"((?:LMGET|HF|OLLAMA|EXO|LOCALAI)_[A-Z0-9_]+|MODELS_PATH)"/g)) covEnv.add(m[1]);
  }
}
const named = (t) => new RegExp(`(^|[^A-Za-z0-9_-])${t.replace(/[$]/g, "\\$")}(?![A-Za-z0-9_-])`).test(answersText);
for (const s of covSubs) if (!new RegExp(`lmget ${s}\\b`).test(answersText)) fail("coverage", `subcommand ${s} is in no answer`);
for (const f of covFlags) if (!named(f)) fail("coverage", `flag ${f} is in no answer`);
for (const v of covEnv) if (!named(v)) fail("coverage", `variable ${v} is in no answer`);

// 7. ids-<lang>.tsv is what ids.mjs writes from this json
let idsNow = null;
try { idsNow = readFileSync(idsPath(lang), "utf8"); } catch {}
if (idsNow !== idsTsv(db)) fail("ids", `${idsPath(lang)} is out of date: run node scripts/chat/ids.mjs`);

// 9. the rules of ask() (spell.js): each one is needed. With "floor",
// "smalltalk-floor" or "quotes" off, some negative gets an answer; every name
// of code.json makes "how does NAME work?" a question about the code, and
// with "code" off none does; config.framedExample is answered and framed,
// and with "framed" off it is not; no test question is framed
let rules = 0;
for (const r of Spell.RULES) {
  rules++;
  if (["floor", "smalltalk-floor", "quotes"].includes(r)) {
    if (!(db.negatives || []).some((q) => chat.ask(q, { rulesOff: [r] }).hit)) fail("rules", `${r}: no negative needs it`);
  }
}
let codeNames = 0;
for (const name of [...Object.keys(chat.code.symbols), ...Object.keys(chat.code.methods), ...Object.keys(chat.code.files)]) {
  codeNames++;
  const q = `how does ${name} work?`;
  if (!(chat.ask(q).code || []).some((r) => r.name === name)) fail("rules", `code: "${q}" is not a question about the code`);
  if (chat.ask(q, { rulesOff: ["code"] }).code) fail("rules", `code: "${q}" is a question about the code with the rule off`);
}
{
  const ex = db.config.framedExample;
  const r = chat.ask(ex || "");
  if (!r.hit || !r.weak) fail("rules", `framed: "${ex}" -> ${r.hit?.id ?? "fallback"}${r.hit ? " unframed" : ""}, want a framed answer`);
  if (chat.ask(ex || "", { rulesOff: ["framed"] }).weak) fail("rules", `framed: "${ex}" is framed with the rule off`);
  for (const e of all) for (const q of e.tests || []) if (chat.ask(q).weak) fail("rules", `framed: test "${q}" (${e.id}) is framed`);
}

// 8. code.json is what code.mjs writes from the tracked files and these
// answers: a stale index misses new code names, or takes a name an answer
// now shows for code
let codeNow = null;
try { codeNow = readFileSync(codePath, "utf8"); } catch {}
if (codeNow !== codeJson()) fail("code", "site/chat/code.json is out of date: run node scripts/chat/code.mjs");
if (!db.config.codeAnswer?.includes("{refs}")) fail("integrity", "config.codeAnswer has no {refs}");
if (!db.config.closestLabel?.includes("{q}")) fail("integrity", "config.closestLabel has no {q}");

console.log(`entries ${db.entries.length} + smalltalk ${(db.smalltalk || []).length}; tests ${tests}; typos ${typoOK}/${typoTotal} (${(typoRate * 100).toFixed(1)}%); heavy typos ${heavyOK}/${heavyTotal} (${(heavyRate * 100).toFixed(1)}%); negatives ${(db.negatives || []).length}; prep rules ${prepRules}; commands checked ${cmds}; coverage ${covSubs.length} subcommands, ${covFlags.size} flags, ${covEnv.size} variables; rules ${rules}, code names ${codeNames}; subcommands known ${subs.size}`);
if (fails.length) {
  console.log(`FAIL (${fails.length})`);
  for (const f of fails) console.log("  " + f);
  process.exit(1);
}
console.log("OK");
