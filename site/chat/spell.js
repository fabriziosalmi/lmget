/* ask() of the lmget chat: a question that names lmget's source code gets
 * the files it is in (code.json); any other goes through the question-form
 * normalisation (prep.js), the matcher, and a spelling repair used only when
 * the matcher finds nothing; an answer must then cover enough of the question
 * (see FRAMED and FLOOR). A word of 6 or more letters that is no keyword becomes the one keyword word
 * within two edits of it (insertion, deletion, substitution, or a swap of two
 * neighbours), with the same first letter; when two are equally close, it is
 * left as it is: a guess would be an invented answer. Needs prep.js: the
 * browser loads it first (window.ZeroPrep) and gets window.ZeroSpell, Node
 * gets module.exports. */
(function (root, factory) {
  if (typeof module === "object" && module.exports) module.exports = factory(require("./prep.js"));
  else root.ZeroSpell = factory(root.ZeroPrep);
})(typeof self !== "undefined" ? self : this, function (Prep) {
  "use strict";

  // optimal string alignment distance, 3 meaning "more than 2"
  function osa(a, b) {
    const n = a.length, m = b.length;
    if (Math.abs(n - m) > 2) return 3;
    const d = [];
    for (let i = 0; i <= n; i++) d.push([i]);
    for (let j = 1; j <= m; j++) d[0][j] = j;
    for (let i = 1; i <= n; i++) {
      for (let j = 1; j <= m; j++) {
        const cost = a[i - 1] === b[j - 1] ? 0 : 1;
        let v = Math.min(d[i - 1][j] + 1, d[i][j - 1] + 1, d[i - 1][j - 1] + cost);
        if (i > 1 && j > 1 && a[i - 1] === b[j - 2] && a[i - 2] === b[j - 1]) v = Math.min(v, d[i - 2][j - 2] + 1);
        d[i][j] = v;
      }
    }
    return d[n][m];
  }

  // the words of every keyword, normalised as the matcher does, 5+ letters
  function vocabulary(db, M) {
    const v = new Set();
    for (const e of db.entries.concat(db.smalltalk || [])) {
      for (const k of e.keywords || []) for (const t of M.tokens(k)) if (t.length >= 5) v.add(t);
    }
    return v;
  }

  // the input with its long unknown words repaired, or null when none was
  function correct(text, vocab, M) {
    let changed = false;
    const out = M.norm(text).split(" ").map((w) => {
      if (w.length < 6 || vocab.has(w)) return w;
      let best = null, bd = 3, ties = 0;
      for (const v of vocab) {
        if (v[0] !== w[0] || Math.abs(v.length - w.length) > 2) continue;
        const d = osa(w, v);
        if (d < bd) { bd = d; best = v; ties = 1; } else if (d === bd) ties++;
      }
      if (best && bd <= 2 && ties === 1) { changed = true; return best; }
      return w;
    });
    return changed ? out.join(" ") : null;
  }

  // The spelling repair is trusted only where a misspelling is the likely
  // story: a short input (at most 2 words that are not stopwords), or a repaired input that
  // scores strongly (a keyword phrase, 1.8 or more). A sentence of ordinary
  // words is never rewritten into a question about lmget.
  const SHORT = 2;
  // the evidence a long question needs (Fab's choice 2, 2026-11-19): with 4
  // or more words that are not stopwords, a score of 2 or more, at least one
  // keyword phrase; measured on the development set: out-of-scope questions
  // refused 72.1% -> 83.6% (en), 63.2% -> 75.0% (it)
  const EVIDENCE_WORDS = 4;
  const EVIDENCE_SCORE = 2.0;
  const STRONG = 1.8;
  // under this coverage an answer says which question it answers (Fab's
  // choice, 2026-11-19): the share of the question's words that are words of
  // the entry (its question, keywords and tests). Measured on the judged
  // development half of the lmget-qabench, 157 answers: at 0.4 it frames 42
  // of 45 wrong answers, 55 of 69 near ones, 11 of 43 right ones; the
  // matcher's score does not tell them apart
  const FRAMED = 0.4;
  // and under this one, in a question of EVIDENCE_WORDS or more, the entry
  // (smalltalk too) does not answer at all: on the same judged half, 0 of 43
  // right answers lost, 26 of 45 wrong ones and 56 of 73 lmget answers to
  // off-topic messages removed
  const FLOOR = 0.25;
  // a quoted span: '...', "...", curly quotes or guillemets, opened after a
  // space or at the start (an apostrophe inside a word opens nothing)
  const QUOTED = /(^|\s)(?:'[^'\n]+'|"[^"\n]+"|\u2018[^\u2019\n]+\u2019|\u201c[^\u201d\n]+\u201d|\u00ab[^\u00bb\n]+\u00bb)/g;
  // the names of lmget's source code a question uses (code.json, written by
  // scripts/chat/code.mjs from the tracked files): a method as Client.Pull, a
  // file's base name as memory.go, a defined name that cannot be an ordinary
  // word as plannedFilesAllowed. At most 3, each with its paths.
  function codeRefs(text, code) {
    const refs = [], seen = new Set();
    const put = (name, paths) => {
      if (seen.has(name) || refs.length >= 3) return;
      seen.add(name);
      refs.push({ name, paths: [].concat(paths) });
    };
    for (const tok of String(text).match(/\w[\w./-]*\w/g) || []) {
      const segs = tok.split(".");
      for (let i = 0; i + 1 < segs.length; i++) {
        const m = segs[i].split("/").pop() + "." + segs[i + 1];
        if (Object.prototype.hasOwnProperty.call(code.methods, m)) {
          put(m, code.methods[m]);
          seen.add(segs[i + 1]); // Client.Pull says it all: not Pull again
        }
      }
      const base = tok.split("/").pop();
      // a file already linked by a name in it says nothing new
      const linked = (paths) => paths.every((p) => refs.some((r) => r.paths.includes(p)));
      if (Object.prototype.hasOwnProperty.call(code.files, base) && !linked(code.files[base])) put(base, code.files[base]);
      for (const part of tok.split(/[./-]/)) {
        if (Object.prototype.hasOwnProperty.call(code.symbols, part)) put(part, code.symbols[part]);
      }
    }
    return refs;
  }

  // the share of the question's words (stopwords aside) that are words of the
  // entry: its question, keywords and tests, in canonical form
  const entryWords = new WeakMap();
  function coverage(entry, q, db, M) {
    let words = entryWords.get(entry);
    if (!words) {
      const lang = db.config && db.config.lang;
      words = [...new Set([entry.question || ""].concat(entry.keywords || [], entry.tests || []).flatMap((s) => M.tokens(Prep.prep(s, lang))))];
      entryWords.set(entry, words);
    }
    const toks = M.tokens(q);
    if (!toks.length) return 1;
    return toks.filter((t) => M.wordBest(t, words) >= 0.8).length / toks.length;
  }

  // the answer to a question about the code: where each name is, linked
  function codeAnswer(refs, db, code) {
    const where = refs.map((r) => "`" + r.name + "`: " + r.paths.slice(0, 3).map((p) => "[" + p + "](" + code.repo + p + ")").join(", "));
    return db.config.codeAnswer.replace("{refs}", where.join("\n- "));
  }

  // the canonical form of the question (prep.js, in the language of db), the
  // matcher on it, then the repaired input when the matcher found nothing.
  // options.code (code.json): a question that names lmget's source code gets
  // { code: refs } and no entry, whatever it matched: the chat's answers are
  // a user's answers. The result carries the entry's score, and weak when it
  // is under FRAMED: the chat then says which question it is answering.
  function ask(text, db, M, vocab, options) {
    // options.prepOff: names of prep rules to skip; options.rulesOff: names
    // of RULES to skip (the gates use both)
    const off = new Set((options && options.rulesOff) || []);
    const code = options && options.code && !off.has("code") ? codeRefs(text, options.code) : [];
    const prepped = Prep.prep(text, db.config && db.config.lang, { off: options && options.prepOff });
    if (code.length) {
      // the user answer that covers the question well, offered after the code
      const near = M.match(prepped, db, options);
      const related = near && !near.id.startsWith("st-") && coverage(near, prepped, db, M) >= FRAMED ? near : null;
      return { hit: null, corrected: null, prepped, code, related };
    }
    const bare = String(text).replace(QUOTED, " ");
    const unquoted = bare !== String(text) ? Prep.prep(bare, db.config && db.config.lang) : null;
    const scored = (hit, q, corrected) => {
      const score = M.scoreEntry(hit, M.norm(q), M.tokens(q));
      const cov = coverage(hit, q, db, M);
      // smalltalk is never framed: "the closest question: are you sure?" is
      // no answer, so a weak smalltalk match is no match
      const small = hit.id.startsWith("st-");
      const floor = small ? (off.has("smalltalk-floor") ? 0 : FRAMED) : off.has("floor") ? 0 : FLOOR;
      if (cov < floor && M.tokens(q).length >= EVIDENCE_WORDS) return { hit: null, corrected: null, prepped, coverage: cov };
      // quoted text is what the message is about, not what it says: "translate
      // 'thank you' into Italian" thanks no one
      if (small && unquoted !== null && !off.has("quotes") && coverage(hit, unquoted, db, M) < FRAMED) return { hit: null, corrected: null, prepped, coverage: cov };
      return { hit, corrected, prepped, score, coverage: cov, weak: !small && !off.has("framed") && cov < FRAMED };
    };
    const hit = M.match(prepped, db, options);
    if (hit) {
      // a long question needs more than one loose word: "write an email to
      // my boss" must not land on an entry because "boss" matched one
      const n = M.tokens(prepped).length;
      if (n >= EVIDENCE_WORDS && M.scoreEntry(hit, M.norm(prepped), M.tokens(prepped)) < EVIDENCE_SCORE) {
        return { hit: null, corrected: null, prepped };
      }
      return scored(hit, prepped, null);
    }
    const fixed = correct(prepped, vocab, M);
    if (!fixed) return { hit: null, corrected: null, prepped };
    const again = M.match(fixed, db, options);
    if (!again) return { hit: null, corrected: null, prepped };
    const short = M.tokens(prepped).length <= SHORT;
    const strong = M.scoreEntry(again, M.norm(fixed), M.tokens(fixed)) >= STRONG;
    return short || strong ? scored(again, fixed, fixed) : { hit: null, corrected: null, prepped };
  }

  // the rules options.rulesOff can name
  const RULES = ["code", "floor", "smalltalk-floor", "quotes", "framed"];

  return { osa, vocabulary, correct, codeRefs, codeAnswer, coverage, ask, RULES };
});
