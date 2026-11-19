// lmget.org: the first visit opens on the install command alone, then an
// iris opens from it onto the page. Once per browser and never again; never with
// reduced motion; never when storage is off. A blocking script in the head,
// so the choice is made before the first paint: nothing flashes. It also
// marks <html class="js">: what needs scripts (the chat) shows from the
// first paint, and never without them.
document.documentElement.classList.add("js");
try {
  if (!localStorage.getItem("lmget-intro") && !matchMedia("(prefers-reduced-motion: reduce)").matches) {
    const root = document.documentElement;
    root.classList.add("intro");
    // the iris opens from the install command: centre it there once the
    // page is parsed, well before it opens (0.6 s)
    document.addEventListener("DOMContentLoaded", () => {
      const r = document.querySelector(".hero .install")?.getBoundingClientRect();
      if (!r) return;
      root.style.setProperty("--iris-x", `${r.left + r.width / 2}px`);
      root.style.setProperty("--iris-y", `${r.top + r.height / 2}px`);
    }, { once: true });
  }
  localStorage.setItem("lmget-intro", "1");
} catch {
  // no storage, no intro: the page shows at once
}
