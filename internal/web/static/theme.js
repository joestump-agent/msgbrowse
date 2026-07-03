// Theme toggle for msgbrowse. Self-hosted (served from /static under
// script-src 'self') so it runs under the strict CSP. Switches the daisyUI
// theme between "slate" (default, dark) and "slate-light" (derived light) and
// persists the choice in localStorage. Loaded in <head> (not deferred) so the
// saved theme is applied before first paint — no flash of the default theme
// (SPEC-0006 REQ-0006-001).
(function () {
  "use strict";
  var KEY = "msgbrowse-theme";
  var DARK = "slate";
  var LIGHT = "slate-light";

  var saved;
  try {
    saved = localStorage.getItem(KEY);
  } catch (e) {
    saved = null;
  }
  if (saved === DARK || saved === LIGHT) {
    document.documentElement.setAttribute("data-theme", saved);
  }

  // Event delegation on document works even though this runs before <body> is
  // parsed: the listener is attached to the document, and the toggle button is
  // matched at click time.
  document.addEventListener("click", function (e) {
    var btn = e.target.closest && e.target.closest("[data-theme-toggle]");
    if (!btn) return;
    var html = document.documentElement;
    var current = html.getAttribute("data-theme") || DARK;
    var next = current === DARK ? LIGHT : DARK;
    // Theme-switch guard (SPEC-0008 REQ-0008-011): .theme-switching disables
    // all CSS transitions (see input.css) while data-theme flips, so the swap
    // is one style recalc instead of thousands of elements animating their
    // colors. Removed on a double rAF: the first rAF fires before the paint
    // that reflects the new theme, the second fires after it has been
    // committed, so transitions come back only once the swap is on screen.
    html.classList.add("theme-switching");
    html.setAttribute("data-theme", next);
    requestAnimationFrame(function () {
      requestAnimationFrame(function () {
        html.classList.remove("theme-switching");
      });
    });
    try {
      localStorage.setItem(KEY, next);
    } catch (e) {
      /* ignore storage failures (private mode) */
    }
  });
})();
