#!/usr/bin/env node
// Runs the lmget-qabench (a separate repository: 1000 lmget questions per
// language, and 500 lmget plus 500 off-topic ones) through the chat, read as
// it is. The bench carries reference answers, not chat ids, so this reports
// what can be counted without judging: answered, code fallback, fallback, and
// for off-topic rows smalltalk and lmget entries (an lmget entry is always
// wrong there). Whether an answer answers is judged outside, on a sample.
//
// The bench is split once, by a hash of each row's id (an off-topic row's id
// is its row number), into a development half that the chat may be tuned on
// and a held-out half that only measures: --half held prints totals, never a
// question.
//
//   node scripts/chat/qabench.mjs [--bench DIR] [--lang en|it] [--set full|mixed]
//        [--half dev|held|all] [--dump FILE]
//
// --dump (dev or all only) writes one JSON line per row: the question, what
// the chat answered, its score, and the bench's reference answer.

import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..");
import { loadChat } from "./load.mjs";
const args = process.argv.slice(2);
const opt = (n, d) => (args.indexOf(n) >= 0 ? args[args.indexOf(n) + 1] : d);
const bench = opt("--bench", join(root, "..", "lmget-qabench"));
const lang = opt("--lang", "en");
const set = opt("--set", "full");
const half = opt("--half", "dev");
const dump = opt("--dump", null);
if (!["dev", "held", "all"].includes(half)) throw new Error("--half dev|held|all");
if (dump && half === "held") throw new Error("the held-out half is never dumped");

const files = {
  full: { en: ["questions.jsonl", "answers.jsonl"], it: ["questions_it.jsonl", "answers_it.jsonl"] },
  mixed: { en: ["questions_mixed_fallback.jsonl", "answers_mixed_fallback.jsonl"], it: ["questions_mixed_fallback_it.jsonl", "answers_mixed_fallback_it.jsonl"] },
}[set]?.[lang];
if (!files) throw new Error("--set full|mixed, --lang en|it");
const jsonl = (p) => readFileSync(p, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l));
const qs = jsonl(join(bench, "q", files[0]));
const as = jsonl(join(bench, "a", files[1]));
if (qs.length !== as.length) throw new Error(`${files[0]} and ${files[1]} differ in length`);

// the split: the same rows in both languages and in both sets
export const inDev = (key) => createHash("sha256").update("lmget-qabench-split-v1:" + key).digest()[0] % 2 === 0;
const keyOf = (q, row) => (q.category === "off_topic" ? "off:" + row : q.id);

const chat = loadChat(lang);
const c = { on: 0, onAnswered: 0, onCode: 0, onFallback: 0, onSmall: 0, off: 0, offFallback: 0, offCode: 0, offSmall: 0, offEntry: 0, weak: 0 };
const out = [];
qs.forEach((q, row) => {
  if (as[row].id !== q.id) throw new Error(`row ${row}: question id ${q.id}, answer id ${as[row].id}`);
  const dev = inDev(keyOf(q, row));
  if ((half === "dev" && !dev) || (half === "held" && dev)) return;
  const r = chat.ask(q.question);
  const got = r.hit ? r.hit.id : r.code ? "code" : "fallback";
  const small = got.startsWith("st-");
  if (r.hit && r.weak) c.weak++;
  if (q.category === "off_topic") {
    c.off++;
    if (got === "fallback") c.offFallback++;
    else if (got === "code") c.offCode++;
    else if (small) c.offSmall++;
    else c.offEntry++;
  } else {
    c.on++;
    if (got === "fallback") c.onFallback++;
    else if (got === "code") c.onCode++;
    else {
      c.onAnswered++;
      if (small) c.onSmall++;
    }
  }
  if (dump) out.push({ row, id: q.id, type: q.type, q: q.question, got, weak: !!r.weak, score: r.score ?? null, code: r.code ?? null, ref: as[row].answer });
});
if (dump) writeFileSync(dump, out.map((o) => JSON.stringify(o)).join("\n") + "\n");
let line = `qabench ${set} ${lang} (${half}): lmget ${c.on}: answered ${c.onAnswered} (smalltalk ${c.onSmall}), code ${c.onCode}, fallback ${c.onFallback}`;
if (c.off) line += ` | off-topic ${c.off}: fallback ${c.offFallback}, code ${c.offCode}, smalltalk ${c.offSmall}, lmget entry ${c.offEntry}`;
line += ` | answers framed as closest ${c.weak}`;
console.log(line);
