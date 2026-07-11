// Copy-to-clipboard for the Connect/Settings page (SPEC-0010). Self-hosted
// (served from /static under script-src 'self') so it runs under the strict
// CSP — no inline handlers.
//
// Any button carrying data-copy-target="<id>" copies the textContent of the
// element with that id. A button carrying data-copy-value="<text>" instead
// copies that literal string — for tiles whose visible text isn't the value to
// copy (e.g. a link pill that shows only the domain but must copy the full URL,
// issue #14), which would otherwise need a hidden per-item element + unique id.
// data-copy-value wins when both are present. Feedback is doubled per the spec's Accessibility
// requirements: visually, the button swaps its copy icon for a check for a
// couple of seconds (.copied class); for assistive tech, the button's
// data-copy-announce text is written into the #copy-announce
// aria-live="polite" region — never visual change alone.
//
// One document-level delegated listener (buttons are native <button>s, so
// keyboard activation — Enter/Space — arrives as this same click event), which
// keeps working across htmx boosted swaps and history restores without
// re-initialization.
(function () {
  "use strict";

  var RESET_MS = 2000;

  function announce(text) {
    var region = document.getElementById("copy-announce");
    if (!region) return;
    // Clear first so copying the same block twice re-announces (live regions
    // only fire on content *changes*).
    region.textContent = "";
    region.textContent = text;
  }

  // Legacy-path copy for engines without the async clipboard API (or non-secure
  // contexts, e.g. a non-loopback bind): select the text in an off-screen
  // textarea and execCommand("copy"). The sr-only class keeps it visually
  // hidden without inline styles (style-src 'self').
  function fallbackCopy(text) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.className = "sr-only";
    document.body.appendChild(ta);
    ta.select();
    var ok = false;
    try {
      ok = document.execCommand("copy");
    } catch (e) {
      ok = false;
    }
    document.body.removeChild(ta);
    return ok;
  }

  function confirmCopied(btn) {
    btn.classList.add("copied");
    announce(btn.getAttribute("data-copy-announce") || "Copied");
    setTimeout(function () {
      btn.classList.remove("copied");
    }, RESET_MS);
  }

  document.addEventListener("click", function (e) {
    var target = e.target;
    if (!target || !target.closest) return;
    // data-copy-value carries the literal text to copy; data-copy-target names
    // an element whose textContent is copied. Prefer the literal when present.
    var btn = target.closest("[data-copy-value],[data-copy-target]");
    if (!btn) return;
    var text;
    if (btn.hasAttribute("data-copy-value")) {
      text = btn.getAttribute("data-copy-value");
    } else {
      var src = document.getElementById(btn.getAttribute("data-copy-target"));
      if (!src) return;
      text = src.textContent.trim();
    }

    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(
        function () {
          confirmCopied(btn);
        },
        function () {
          if (fallbackCopy(text)) confirmCopied(btn);
        }
      );
    } else if (fallbackCopy(text)) {
      confirmCopied(btn);
    }
  });
})();
