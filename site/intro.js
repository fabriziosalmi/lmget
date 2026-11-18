// lmget.org: the first visit opens on the install command alone, then the
// rest of the page fades in. Once per browser and never again; never with
// reduced motion; never when storage is off. A blocking script in the head,
// so the choice is made before the first paint: nothing flashes.
try {
  if (!localStorage.getItem("lmget-intro") && !matchMedia("(prefers-reduced-motion: reduce)").matches) {
    document.documentElement.classList.add("intro");
  }
  localStorage.setItem("lmget-intro", "1");
} catch {
  // no storage, no intro: the page shows at once
}
