/* Question-form normalisation for the lmget chat, applied before the matcher.
 * Many surface forms of one question become one canonical form per language:
 * "cos'è", "cosè", "che cos e", "cosa è" are one question; so are "who made",
 * "who wrote", "who is behind". Only frequent function words and fixed
 * phrases are rewritten, never a content word: a guess would be an invented
 * question. The canonical forms are a few tokens no visitor types, wrapped in
 * z...z so that no keyword can match a part of one:
 *
 *   zwhatz   "what is", asked with no subject but lmget ("what is this?")
 *   zwhoz    "who made / maintains / is behind"
 *   zyouz    "you", when the visitor talks to the chat itself ("are you", "sei")
 *   zemptyz  an input with no letter or digit ("?", "...")
 *   zusez    "how do I use it", "come si usa", with no subject but lmget
 *   zwherez  "where does it put the models", "dove li mette"
 *   zfromz   "where does it download from": the sources lmget knows
 *   zoffz    a question about work lmget never does (training, fine-tuning,
 *            quantizing, configuring a model, buying hardware) that does not
 *            name lmget: no keyword knows it, so it gets the fallback
 *   zllamaz  the model family Llama: the matcher's typo tolerance reads
 *            "llama" as "ollama", so a question about Llama would get the
 *            Ollama answer; llama.cpp, llama-server and llamafile stay
 *
 * and a question with no subject on lmget's site is about lmget ("where do I
 * download it?" is "install lmget"). Every rule has an example that answers
 * with its entry only when the rule is on; scripts/chat/check.mjs proves it.
 * No dependencies; the browser gets window.ZeroPrep, Node gets module.exports. */
(function (root, factory) {
  if (typeof module === "object" && module.exports) module.exports = factory();
  else root.ZeroPrep = factory();
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";

  // words that add nothing to a question about lmget itself: "what is this
  // thing exactly?", "cos'è sto coso in breve?"
  const NOSUBJ_EN = "(?:\\s+(?:it|this|that|thing|tool|program|project|site|here|lmget|exactly|really|actually|even|then|for|about|all|so|in|one|short|simple|plain|words|sentence|briefly|nutshell|please|again|is|does|do|mean|to|me|tell))*";
  const NOSUBJ_IT = "(?:\\s+(?:questo|questa|sto|sta|coso|cosa|roba|programma|strumento|progetto|sito|lmget|di|preciso|esattamente|allora|poi|ma|quindi|in|breve|due|poche|parole|semplici|sintesi|parola|una|per|favore|e|fa|esattamente|mi|spieghi|spiegate|dire|dici))*";

  // the same words before the question: "lmget, cos'è?", "mi spiegate in breve cos'è?"
  const PRE_IT = NOSUBJ_IT.replace("(?:\\s+(?:", "(?:(?:").replace("))*", ")\\s+)*");

  // [name, pattern, replacement, example, expected id of the example]
  const RULES = {
    en: [
      ["en-empty", /^\s*$/, "zemptyz", "??", "st-empty"],
      ["en-offtopic", /^(?!.*\blmget\b).*\b(?:fine\s*-?\s*tun\w*|finetun\w*|train(?:ing)?\s+(?:a\s+|my\s+|the\s+|your\s+)?(?:own\s+)?(?:model|lora|llm|network)s?|train\s+(?:it\s+)?(?:from\s+scratch|on\s+my)|lora\s+on|quantiz\w+\s+(?:a|my|the|this)\s+model|modelfile|temperature|should\s+i\s+buy|buy\s+(?:a|an)\s+(?:new\s+)?(?:gpu|graphics|mac|laptop|pc|card)|(?:install|instal|reinstall|set\s+up|setup|update|upgrade|uninstall)\s+(?:my\s+|the\s+)?(?:ollama|docker|python3?|pip|nginx|vllm|pytorch|torch|cuda|llama\.?\s*cpp|lm\s*studio|comfyui|open\s*webui|nodejs|node|homebrew|brew|git|nvidia|drivers?|transformers))\b.*$/, "zoffz", "how do I train a lora for comfyui", "fallback"],
      ["en-dont", /\b(do|does|did|is|are|was|were|ca|could|would|should|have|has|wo)n'?t\b/g, (m, v) => (v === "ca" ? "can" : v === "wo" ? "will" : v) + " not", "omlx doesnt see the new model", "run-apps"],
      ["en-whats", /\b(what|where|who|how|that|there)'?s\b/g, "$1 is", "what's lmget", "what-is"],
      ["en-what-variants", /\b(?:wut|wat|wht|waht|whta|wtf)\b/g, "what", "wut is lmget", "what-is"],
      ["en-name-only", /^lmget$/, "zwhatz lmget", "lmget?", "what-is"],
      ["en-what-bare", new RegExp("^(?:(?:so|ok|and|but|hi|hello|hey)\\s+)*(?:(?:could|can|would)\\s+(?:you|u)\\s+)?(?:please\\s+)?(?:tell\\s+me\\s+|explain\\s+)?(?:briefly\\s+|quickly\\s+)?(?:what\\s+(?:is|are|does|do)|what(?=\\s+(?:lmget|it|this)\\b)|explain|describe|tell\\s+me\\s+about)" + NOSUBJ_EN + "\\s*$"), "zwhatz lmget", "Hello, could you tell me briefly what lmget is for?", "what-is"],
      ["en-how-work", new RegExp("^(?:so\\s+|ok\\s+|and\\s+)?how\\s+(?:does|do)\\s+(?:it|this|lmget|this\\s+tool|this\\s+thing)\\s+(?:work|works)" + NOSUBJ_EN + "\\s*$"), "zwhatz lmget", "how does it work?", "what-is"],
      ["en-how-use", new RegExp("^(?:so\\s+|ok\\s+|and\\s+)?how\\s+(?:do\\s+(?:i|we|you)|can\\s+i|to|does\\s+one|should\\s+i|would\\s+i|i)\\s+(?:use|start|begin|get\\s+started)(?:\\s+(?:with|using))?" + NOSUBJ_EN + "\\s*$"), "zusez lmget", "how do I use it", "first-download"],
      ["en-where-put", /^(?:so\s+|ok\s+|and\s+)?(?:where|wher|were)\s+(?:are\s+(?:the\s+|my\s+)?(?:models?|files?|downloads?|weights)\s+(?:saved|stored|kept|put|placed|downloaded)|(?:does|do|will|did)\s+(?:it|lmget|this)\s+(?:save|put|store|keep|place|download|write|install)|(?:do|will|did)\s+(?:the\s+)?(?:models?|files?|downloads?|weights)\s+(?:go|end\s+up|get\s+saved|land|live))(?:\s+(?:the\s+)?(?:models?|files?|downloads?|weights|things|stuff|them|it|everything))?(?:\s+(?:to|in|after\s+download(?:ing)?))?\s*$/, "zwherez", "where does it save?", "targets"],
      ["en-llama", /\bllama(?!\s*(?:cpp|server|file|\.cpp))\b/g, "zllamaz", "how long is the context of llama 3", "fallback"],
      ["en-from", /^(?:so\s+|and\s+)?(?:where|which\s+sites?|what\s+sites?)\s+(?:does|can|do|will)\s+(?:it|lmget|this)\s+(?:download|get|fetch|pull|take)(?:\s+(?:models|files|things|stuff))?\s+from\s*$/, "zfromz", "where does it download from", "sources"],
      // a question opened on "it", on lmget's site, is about lmget ("this"
      // is often the chat itself; "does it run on" asks about platforms)
      ["en-it-subject", /^(?:is|does|can|will|could|would)\s+it\b(?!\s+run\s+on\b)/, (m) => m.split(/\s+/)[0] + " lmget", "is it stable", "status"],
      ["en-cost", /^(?:so\s+|and\s+)?(?:how\s+much\s+(?:does\s+(?:it|lmget|this)\s+cost|is\s+(?:it|lmget|this))|is\s+(?:it|lmget|this)\s+(?:paid|expensive)|do\s+i\s+(?:have|need)\s+to\s+pay(?:\s+for\s+(?:it|lmget))?|does\s+(?:it|lmget)\s+cost\s+(?:anything|money))\s*$/, "cost lmget", "how much does it cost", "lmget-license"],
      ["en-store-path", /\bdotlmget\b/g, "lmget store", "whats in ~/.lmget", "store"],
      ["en-ci", /\bci\s*(?:\/\s*)?cd\b|\bci\b/g, "cicd", "lmget in ci", "install-ci"],
      ["en-who-you", /\bwho\s+(?:made|built|wrote|created|programmed|coded|trained|designed)\s+(?:you|this\s+(?:chat|chatbot|bot|assistant))\b/g, "zwhoz zyouz", "who programmed you", "st-who-made-you"],
      ["en-who", /\bwho\s+(?:is\s+|are\s+)?(?:made|wrote|created|built|builds|develops|developed|maintains|maintained|owns|behind|the\s+(?:author|authors|maintainer|maintainers|developer|developers|creator|owner))\b/g, "zwhoz", "who wrote it", "who-made"],
      ["en-you", /\b(?:(?:are|r)\s+(?:you|u)|(?:you|u)\s+(?:are|r)|you\s*'\s*re|youre)\b/g, "zyouz", "are you sure", "st-sure"],
      ["en-goodnight", /\bgood\s*'?\s*(?:night|nite)\b|\bnight\s+night\b/g, "goodnight", "good night", "st-bye"],
      ["en-get-it", new RegExp("^(?:where\\s+(?:can|do|should)\\s+i\\s+(?:get|download|find|grab)|where\\s+(?:to|can\\s+one)\\s+(?:get|download|find)|where\\s+is\\s+the\\s+download|download\\s+link)" + NOSUBJ_EN + "(?:\\s+from)?\\s*$"), "install lmget", "where can i download it", "install"],
      ["en-open", /^(?:is\s+(?:it|this|lmget)|it\s*'?\s*s|lmget\s+is|lmget)\s+open\s*$/, "open source lmget", "is it open", "lmget-license"],
    ],
    it: [
      ["it-empty", /^\s*$/, "zemptyz", "??", "st-empty"],
      ["it-fused-ha", /\b(l'?)?ha(fatto|creato|scritto|sviluppato|programmato|inventato|realizzato)\b/g, (m, l, v) => (l ? "l'ha " : "ha ") + v, "chi l'hafatto", "who-made"],
      ["it-name-only", /^lmget$/, "zwhatz lmget", "lmget?", "what-is"],
      ["it-what-bare", new RegExp("^(?:(?:ma|e|ok|allora|quindi|ciao|salve|buongiorno|buonasera)\\s+)*" + PRE_IT + "(?:(?:che\\s+)?(?:cosa|cos)\\s*'?\\s*e|cose'?|che\\s+(?:roba|cosa)\\s+e|(?:a\\s+)?(?:cosa|che)\\s+serve|(?:che\\s+)?cosa\\s+fa)" + NOSUBJ_IT + "\\s*$"), "zwhatz lmget", "cosa è lmget?", "what-is"],
      ["it-how-work", new RegExp("^(?:ma\\s+|e\\s+|ok\\s+|allora\\s+)?come\\s+funziona" + NOSUBJ_IT + "\\s*$"), "zwhatz lmget", "come funziona?", "what-is"],
      ["it-how-use", new RegExp("^(?:ma\\s+|e\\s+|ok\\s+|allora\\s+)?come\\s+(?:si\\s+usa|lo\\s+uso|si\\s+utilizza|lo\\s+utilizzo|si\\s+comincia|si\\s+inizia|inizio|comincio|lo\\s+si\\s+usa|si\\s+adopera)" + NOSUBJ_IT + "\\s*$"), "zusez lmget", "come si usa?", "first-download"],
      ["it-where-put", /^(?:ma\s+|e\s+|allora\s+)?(?:dove|dov'\s*e|dov\s+e)\s+(?:e\s+)?(?:che\s+)?(?:li\s+|le\s+|lo\s+|la\s+|me\s+li\s+)?(?:salva|mette|scarica|finiscono|vanno|installa|tiene|mettete|salvate|archivia)(?:\s+(?:i|il|le|gli|tutti))?(?:\s+(?:modelli|modello|file|pesi|download|dati))?(?:\s+(?:scaricati|che\s+scarico|scaricati\s+da\s+lmget))?\s*$/, "zwherez", "dov'è che li salva?", "targets"],
      ["it-llama", /\bllama(?!\s*(?:cpp|server|file|\.cpp))\b/g, "zllamaz", "quanto è lungo il contesto di llama 3", "fallback"],
      ["it-cost", /^(?:ma\s+)?(?:quanto\s+costa(?:\s+lmget)?|costa\s+qualcosa|si\s+paga|e\s+a\s+pagamento|bisogna\s+pagare|devo\s+pagare|e\s+gratis|e\s+gratuito)\s*$/, "costo lmget", "quanto costa?", "lmget-license"],
      ["it-store-path", /\bdotlmget\b/g, "store di lmget", "cos'è ~/.lmget", "store"],
      ["it-ci", /\bci\s*\/?\s*cd\b|\b(?:di|nella|in|della|sulla|per\s+la|la)\s+ci\b|^ci$/g, (m) => m.replace(/ci\s*\/?\s*cd|ci$/, "cicd"), "funziona nella ci?", "install-ci"],
      ["it-who-you", /\bchi\s+(?:ti|vi)\s+(?:ha|hanno)\s+(?:fatto|creato|scritto|programmato|sviluppato|realizzato|addestrato)\b/g, "zwhoz zyouz", "chi ti ha programmato", "st-who-made-you"],
      ["it-who", /\bchi\s+(?:(?:(?:l'\s*|l\s+|lo\s+|la\s+)?(?:ha|hanno)|lha|la)\s+(?:fatto|creato|scritto|sviluppato|programmato|inventato|realizzato)|c'?\s*e\s+dietro|(?:lo\s+|la\s+)?(?:mantiene|sviluppa|gestisce|cura)|e\s+(?:l'?\s*autore|lo\s+sviluppatore|il\s+creatore|il\s+responsabile)|sono\s+gli\s+(?:autori|sviluppatori))\b/g, "zwhoz", "chi l'ha fatto?", "who-made"],
      // "sei" is also "six": not before a number or a unit
      ["it-you", /\bsei\b(?!\s*(?:gb|giga|gigabyte|mb|tb|mesi|giorni|anni|ore|minuti|secondi|volte|core|modelli|file|euro|\d))/g, "zyouz", "sei sicuro?", "st-sure"],
      ["it-offtopic", /^(?!.*\blmget\b).*\b(?:fine\s*-?\s*tun\w*|finetun\w*|(?<!\bti\s)addestr(?!arti\b)\w*|allenare\s+(?:un|il|una)\s+(?:modello|lora)|quantizzar\w*\s+(?:un|il|questo)\s+modello|quantizzo\s+(?:un|il)\s+modello|modelfile|temperatura|dovrei\s+comprare|devo\s+comprare|che\s+(?:gpu|scheda)\s+compro|comprare\s+una?\s+(?:gpu|scheda|mac|portatile|pc)|(?:installo|installare|installa|intallo|instalo|configuro|configurare|aggiorno|aggiornare|aggiorna|disinstallo|disinstallare)\s+(?:il\s+|i\s+|lo\s+)?(?:ollama|docker|python3?|pip|nginx|vllm|pytorch|torch|cuda|llama\.?\s*cpp|lm\s*studio|comfyui|open\s*webui|nodejs|node|homebrew|brew|git|nvidia|driver|transformers))\b.*$/, "zoffz", "come addestro una lora per comfyui", "fallback"],
      // "è chatgpt?", "è un bot?": on this chat, a question about the chat
      ["it-is-bot", /^(?:ma\s+)?e\s+(?=(?:un\s+|una\s+|un'\s*)?(?:bot|chatbot|robot|ai|ia|chatgpt|chat\s+gpt|gpt|openai|claude|anthropic|gemini|zllamaz|mistral|qwen|deepseek|umano|una\s+persona)\b)/, "zyouz ", "è claude?", "st-claude"],
      ["it-yes", /^(?:si|sii|sisi|si\s+si)$/, "certo", "sì", "st-yes"],
      ["it-goodnight", /\bbuona\s*'?\s*notte\b/g, "buonanotte", "buona notte", "st-bye"],
      // impersonal or first person only: "dove scarica?" (where does it
      // download to) is another question
      ["it-get-it", new RegExp("^(?:da\\s+)?dove\\s+(?:si\\s+(?:scarica|trova|prende|installa)|(?:lo\\s+|la\\s+)?(?:scarico|trovo|prendo)|(?:posso|devo)\\s+(?:scaricarlo|trovarlo|prenderlo|scaricare|trovare|prendere)|scaricarlo|trovarlo|prenderlo)" + NOSUBJ_IT + "\\s*$"), "installare lmget", "dove si scarica?", "install"],
      ["it-open", /^(?:e|lmget\s+e|e\s+lmget)\s+open\s*$/, "open source lmget", "è open ?", "lmget-license"],
    ],
  };

  // lowercase, no accents, typographic apostrophes as ', punctuation as
  // space: what the matcher would see, with the apostrophe kept for the rules
  function clean(text) {
    return String(text || "").toLowerCase()
      .normalize("NFD").replace(/[̀-ͯ]/g, "")
      .replace(/[‘’`´]/g, "'")
      .replace(/(?:~\/|\$home\/|\/)?\.lmget\b/g, " dotlmget ")
      .replace(/[^a-z0-9'\s]/g, " ")
      .replace(/\s+/g, " ")
      .trim();
  }

  // the canonical form of a question; options.off: names of rules to skip
  // (the gates use it to prove each rule is needed)
  function prep(text, lang, options) {
    const off = (options && options.off) || null;
    let s = clean(text);
    for (const [name, re, to] of RULES[lang] || []) {
      if (off && off.has(name)) continue;
      if (re.global) re.lastIndex = 0;
      s = s.replace(re, to).replace(/\s+/g, " ").trim();
    }
    return s;
  }

  return { prep, clean, RULES };
});
