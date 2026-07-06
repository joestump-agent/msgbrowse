// Below-md drawer toggle (#175, reduced by #190). Self-hosted (served from
// /static under script-src 'self') so it runs under the strict CSP — no inline
// handlers. The header burger (the #nav-drawer <label>, md:hidden) only exists
// below md, where the native label→checkbox toggle drives the daisyUI overlay
// drawer; the #175 lg+ persistent-collapse feature (the .sidebar-collapsed
// <html> class and its localStorage key) is retired — at md+ the sidebar is
// simply always pinned (md:drawer-open) and there is no burger.
//
// Keyboard: a <label> has no native focus or key activation, so the template
// gives the burger role="button" + tabindex="0" and the keydown handler below
// maps Enter/Space to a synthetic click. aria-expanded mirrors the drawer
// checkbox.
//
// Listeners live on document, so they survive boosted #main-content swaps AND
// htmx:historyRestore (which replaces body.innerHTML — document-level
// delegation is untouched) with no re-init hook needed.
(function () {
  "use strict";
  // Scoped to the header burger: the drawer-overlay close label targets the
  // same checkbox but needs no key/aria handling of its own.
  var BURGER = '.app-toolbar label[for="nav-drawer"]';

  // Drop the retired #175 collapse key so it can never confuse a future
  // reader of localStorage (the class and CSS it drove are gone).
  try {
    localStorage.removeItem("msgbrowse-sidebar-collapsed");
  } catch (e) {
    /* ignore storage failures (private mode) */
  }

  // aria-expanded mirrors the drawer checkbox (the only mechanism left).
  function syncExpanded() {
    var burger = document.querySelector(BURGER);
    if (!burger) return;
    var box = document.getElementById("nav-drawer");
    burger.setAttribute("aria-expanded", box && box.checked ? "true" : "false");
  }

  // Enter/Space on the focused burger behave like a click (see the keyboard
  // note in the header comment). preventDefault keeps Space from scrolling.
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Enter" && e.key !== " ") return;
    var burger = e.target.closest && e.target.closest(BURGER);
    if (!burger) return;
    e.preventDefault();
    burger.click();
  });

  // The native label→checkbox toggle (and the drawer-overlay close label)
  // change drawer state without any handler of ours — track it here.
  document.addEventListener("change", function (e) {
    if (e.target && e.target.id === "nav-drawer") syncExpanded();
  });

  document.addEventListener("DOMContentLoaded", syncExpanded);
  // htmx:historyRestore replaces body.innerHTML from a snapshot that may carry
  // a stale aria-expanded value.
  document.addEventListener("htmx:historyRestore", syncExpanded);
})();
