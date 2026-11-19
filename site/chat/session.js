/* One conversation with the lmget chat, the same in the terminal
 * (scripts/chat/chat.mjs) and on lmget.org (ui.js): an entry's answers in
 * turn, the thread's entry for short follow-ups, the fallbacks in turn, the
 * code answer, and the frame of a weak match. Needs the matcher (M) and
 * spell.js (Spell); the browser gets window.ZeroSession, Node gets
 * module.exports. */
(function (root, factory) {
  if (typeof module === "object" && module.exports) module.exports = factory();
  else root.ZeroSession = factory();
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";

  // db: <lang>.json; code: code.json, or null for no code answers
  function session(db, code, M, Spell) {
    const vocab = Spell.vocabulary(db, M);
    const byId = new Map(db.entries.concat(db.smalltalk || []).map((e) => [e.id, e]));
    const cursor = Object.create(null); // answer variant per entry
    let ctx = null; // the thread's entry
    let fallbackAt = 0;

    // { id: entry id, "code", or null for the fallback; text: markdown;
    //   suggest: [{ id, question }]; result: what ask() returned }
    function answer(text) {
      const r = Spell.ask(text, db, M, vocab, { ctxEntry: ctx, code });
      let id = null, out, next = [];
      if (r.code) {
        id = "code";
        out = Spell.codeAnswer(r.code, db, code);
        if (r.related) next = [r.related.id];
      } else if (!r.hit) {
        out = db.fallbacks[fallbackAt++ % db.fallbacks.length];
      } else {
        id = r.hit.id;
        const n = cursor[id] || 0;
        cursor[id] = n + 1;
        if (!id.startsWith("st-")) ctx = r.hit;
        out = r.hit.answers[n % r.hit.answers.length];
        // a weak match says which question it answers, so a near answer is
        // not taken for the answer
        if (r.weak) out = db.config.closestLabel.replace("{q}", () => r.hit.question) + "\n\n" + out;
        next = r.hit.suggest || [];
      }
      const suggest = next.map((s) => byId.get(s)).filter(Boolean).map((e) => ({ id: e.id, question: e.question }));
      return { id, text: out, suggest, result: r };
    }

    return {
      answer,
      reset() {
        ctx = null;
      },
    };
  }

  return { session };
});
