/* Spelling repair for the lmget chat, used only when the matcher finds nothing.
 * A word of 6 or more letters that is no keyword becomes the one keyword word
 * within two edits of it (insertion, deletion, substitution, or a swap of two
 * neighbours), with the same first letter; when two are equally close, it is
 * left as it is: a guess would be an invented answer. No dependencies; the
 * browser gets window.ZeroSpell, Node gets module.exports. */
(function (root, factory) {
  if (typeof module === "object" && module.exports) module.exports = factory();
  else root.ZeroSpell = factory();
})(typeof self !== "undefined" ? self : this, function () {
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

  // the matcher, then the repaired input when the matcher found nothing. The
  // repair is trusted only where a misspelling is the likely story: a short
  // input (at most 2 words that are not stopwords), or a repaired input that
  // scores strongly (a keyword phrase, 1.8 or more). A sentence of ordinary
  // words is never rewritten into a question about lmget.
  const SHORT = 2;
  const STRONG = 1.8;
  function ask(text, db, M, vocab, options) {
    const hit = M.match(text, db, options);
    if (hit) return { hit, corrected: null };
    const fixed = correct(text, vocab, M);
    if (!fixed) return { hit: null, corrected: null };
    const again = M.match(fixed, db, options);
    if (!again) return { hit: null, corrected: null };
    const short = M.tokens(text).length <= SHORT;
    const strong = M.scoreEntry(again, M.norm(fixed), M.tokens(fixed)) >= STRONG;
    return short || strong ? { hit: again, corrected: fixed } : { hit: null, corrected: null };
  }

  return { osa, vocabulary, correct, ask };
});
