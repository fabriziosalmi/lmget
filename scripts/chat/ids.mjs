#!/usr/bin/env node
// Writes scripts/chat/ids-<lang>.tsv from site/chat/<lang>.json: one line per
// entry and smalltalk, `id<TAB>question`, for whoever writes an evaluation
// set (eval.mjs --file) without reading the keywords. check.mjs fails when a
// file is not what this script would write, so they never drift.
//
//   node scripts/chat/ids.mjs [--lang en|it]   (both languages without --lang)

import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..");

export function idsTsv(db) {
  return [...db.entries, ...(db.smalltalk || [])].map((e) => `${e.id}\t${e.question}`).join("\n") + "\n";
}

export const idsPath = (lang) => join(here, `ids-${lang}.tsv`);

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const langs = args.includes("--lang") ? [args[args.indexOf("--lang") + 1]] : ["en", "it"];
  for (const lang of langs) {
    const db = JSON.parse(readFileSync(join(root, "site", "chat", `${lang}.json`), "utf8"));
    writeFileSync(idsPath(lang), idsTsv(db));
    console.log(`${idsPath(lang)}: ${db.entries.length + (db.smalltalk || []).length} ids`);
  }
}
