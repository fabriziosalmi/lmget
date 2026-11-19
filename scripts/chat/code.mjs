#!/usr/bin/env node
// Writes site/chat/code.json: the names a question about lmget's source code
// uses, each with the file it is in, so the chat can tell such a question
// from a user's and point to the file instead of answering with the nearest
// user answer. Read from the tracked files only (git ls-files):
//
// - files: the base name of every tracked file, with its paths;
// - symbols: functions, types, constants and variables defined in the Go,
//   Python, JavaScript and shell files whose names cannot be ordinary words
//   (a lower-to-upper hump, as plannedFilesAllowed, ChooseVariant, or
//   lower_snake_case, as check_bom);
// - methods: Type.Method of every Go method, as Client.AliasesURL.
//
// A name any answer or question of the chat (en.json, it.json) shows is a
// user's name, not code (lmget.lock, install.sh, LMGET_ORIGINS), and is left
// out. check.mjs fails when code.json is not what this script writes.
//
//   node scripts/chat/code.mjs

import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join, basename } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..");
export const codePath = join(root, "site", "chat", "code.json");
const REPO = "https://github.com/fabriziosalmi/lmget/blob/main/";

// a defined name that cannot be an ordinary word
export const codeLooking = (n) => n.length >= 5 && (/[a-z][A-Z]/.test(n) || /^[a-z][a-z0-9]*_[a-z0-9_]+$/.test(n));

function userText() {
  const parts = [];
  for (const lang of ["en", "it"]) {
    const db = JSON.parse(readFileSync(join(root, "site", "chat", `${lang}.json`), "utf8"));
    parts.push(...Object.values(db.config).filter((v) => typeof v === "string"), ...db.fallbacks);
    for (const e of [...db.entries, ...(db.smalltalk || [])]) parts.push(e.question, ...e.answers);
  }
  return parts.join("\n");
}

export function codeJson() {
  const text = userText();
  const shown = (name) => new RegExp(`(^|[^A-Za-z0-9_.-])${name.replace(/[.$]/g, "\\$&")}(?![A-Za-z0-9_-])`).test(text);
  const tracked = execFileSync("git", ["ls-files"], { cwd: root, encoding: "utf8" }).split("\n").filter(Boolean);
  const files = {};
  const symbols = {};
  const methods = {};
  const add = (map, name, path) => {
    if (!(name in map) && !shown(name)) map[name] = path;
  };
  for (const path of tracked.sort()) {
    const base = basename(path);
    if (base.includes(".") && !base.startsWith(".") && !shown(base)) (files[base] ??= []).push(path);
    const ext = base.split(".").pop();
    if (!["go", "py", "js", "mjs", "sh"].includes(ext)) continue;
    const src = readFileSync(join(root, path), "utf8");
    const names = [];
    if (ext === "go") {
      for (const m of src.matchAll(/^func (?:\(\w*\s*\*?(\w+)(?:\[[^\]]*\])?\)\s*)?(\w+)/gm)) {
        names.push(m[2]);
        if (m[1]) add(methods, `${m[1]}.${m[2]}`, path);
      }
      for (const m of src.matchAll(/^(?:type|const|var) (\w+)/gm)) names.push(m[1]);
      for (const b of src.matchAll(/^(?:type|const|var) \(\n([\s\S]*?)^\)/gm)) {
        for (const m of b[1].matchAll(/^\t(\w+)/gm)) names.push(m[1]);
      }
    } else if (ext === "py") {
      for (const m of src.matchAll(/^\s*(?:def|class) (\w+)/gm)) names.push(m[1]);
    } else if (ext === "sh") {
      for (const m of src.matchAll(/^(\w+)\s*\(\)/gm)) names.push(m[1]);
    } else {
      for (const m of src.matchAll(/^\s*(?:export )?(?:async )?function\*? ([\w$]+)/gm)) names.push(m[1]);
      for (const m of src.matchAll(/^\s*(?:export )?(?:const|let) ([\w$]+)\s*=/gm)) names.push(m[1]);
    }
    for (const n of names) if (codeLooking(n)) add(symbols, n, path);
  }
  const sorted = (o) => Object.fromEntries(Object.entries(o).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));
  return JSON.stringify({ repo: REPO, files: sorted(files), symbols: sorted(symbols), methods: sorted(methods) }, null, 0) + "\n";
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const out = codeJson();
  writeFileSync(codePath, out);
  const j = JSON.parse(out);
  console.log(`${codePath}: ${Object.keys(j.files).length} files, ${Object.keys(j.symbols).length} symbols, ${Object.keys(j.methods).length} methods, ${out.length} bytes`);
}
