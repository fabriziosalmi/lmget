// The chat as every script uses it: the answers of one language
// (site/chat/<lang>.json), the code index (site/chat/code.json), and ask()
// with both, the same pipeline the site runs.
import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
export const root = join(here, "..", "..");
const req = createRequire(import.meta.url);
export const M = req(join(root, "site", "chat", "matcher.js"));
export const Spell = req(join(root, "site", "chat", "spell.js"));
export const Prep = req(join(root, "site", "chat", "prep.js"));

export function loadChat(lang) {
  const db = JSON.parse(readFileSync(join(root, "site", "chat", `${lang}.json`), "utf8"));
  const code = JSON.parse(readFileSync(join(root, "site", "chat", "code.json"), "utf8"));
  const vocab = Spell.vocabulary(db, M);
  // options: ctxEntry (the thread), prepOff (gates), code: false (gates)
  const ask = (q, options = {}) => Spell.ask(q, db, M, vocab, { ...options, code: options.code === false ? null : code });
  return { db, code, vocab, ask };
}
