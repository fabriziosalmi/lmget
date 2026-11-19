#!/usr/bin/env node
// Held-out evaluation of the lmget.org chat: questions written by someone who
// never saw the keywords or the tests, frozen in scripts/chat/eval-dev-<lang>.txt,
// or any file in the same format given with --file:
// `expected-id<TAB>question` (`fallback` for out-of-scope questions).
// It reports, never fails: the gates are check.mjs. The same pipeline the
// chat uses answers each question (ask(): normalisation, matcher, spelling).
//
//   node scripts/chat/eval.mjs [--lang en|it] [--file PATH] [--misses]

import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..");
const req = createRequire(import.meta.url);
const M = req(join(root, "site", "chat", "matcher.js"));
const Spell = req(join(root, "site", "chat", "spell.js"));
const args = process.argv.slice(2);
const opt = (n, d) => (args.indexOf(n) >= 0 ? args[args.indexOf(n) + 1] : d);
const lang = opt("--lang", "en");
const showMisses = args.includes("--misses");
const file = opt("--file", join(here, `eval-dev-${lang}.txt`));
const db = JSON.parse(readFileSync(join(root, "site", "chat", `${lang}.json`), "utf8"));
const vocab = Spell.vocabulary(db, M);
const ids = new Set([...db.entries, ...(db.smalltalk || [])].map((e) => e.id));
// a question that is also a test (or a negative) was tuned on: the rate on
// the other lines is the one that says how the chat does on new questions
const seen = new Set([...db.entries, ...(db.smalltalk || [])].flatMap((e) => e.tests).concat(db.negatives || []).map((t) => t.toLowerCase().trim()));

const lines = readFileSync(file, "utf8").split("\n").filter(Boolean);
let inTotal = 0, inOK = 0, inFallback = 0, outTotal = 0, outOK = 0, unknown = 0, newTotal = 0, newOK = 0;
const misses = [];
for (const line of lines) {
  const [want, q] = line.split("\t");
  const got = Spell.ask(q, db, M, vocab).hit?.id ?? "fallback";
  if (!seen.has(q.toLowerCase().trim())) {
    newTotal++;
    if (got === want) newOK++;
  }
  if (want === "fallback") {
    outTotal++;
    if (got === "fallback") outOK++;
    else misses.push(`fallback <- ${got}\t${q}`);
    continue;
  }
  inTotal++;
  if (!ids.has(want)) unknown++;
  if (got === want) inOK++;
  else {
    if (got === "fallback") inFallback++;
    misses.push(`${want} <- ${got}\t${q}`);
  }
}
const pct = (a, b) => (b ? ((a / b) * 100).toFixed(1) : "-") + "%";
console.log(
  `eval ${lang} (${file.split("/").pop()}): ${lines.length} questions; hit rate ${inOK + outOK}/${lines.length} (${pct(inOK + outOK, lines.length)}); ` +
    `in scope ${inOK}/${inTotal} (${pct(inOK, inTotal)}), of which ${inFallback} got the fallback; ` +
    `fallback precision ${outOK}/${outTotal} (${pct(outOK, outTotal)}); ` +
    `not identical to a test or negative: ${newOK}/${newTotal} (${pct(newOK, newTotal)}); expected ids not in ${lang}.json: ${unknown}`,
);
if (showMisses) for (const m of misses) console.log("  " + m);
