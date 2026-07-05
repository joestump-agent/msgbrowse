// Desktop-shell bridges (SPEC-0010 "Native shell affordances"): the drag
// region (issue #165) and the external-link opener (issue #179).
//
// With the macOS title bar hidden (mac.TitleBarHiddenInset in the shell), the
// unified toolbar is the window's drag surface. Wails' own drag protocol reads
// the --wails-draggable CSS custom property on mousedown and posts the "drag"
// message to the native side (performWindowDragWithEvent) — but the JS runtime
// that implements that reader is injected only into pages served through the
// Wails asset handler, and msgbrowse serves the real app over loopback HTTP
// (SPEC-0010 design decision), so those pages never receive it. This file is
// the missing reader: the same computed-style check and the same "drag"
// message over the webview's script-message bridge, self-hosted under the
// app's strict CSP (script-src 'self'; no inline JS).
//
// External links (issue #179) have the same root cause: the webview has no
// new-window handler, so a target="_blank" navigation to another origin is
// silently dropped. The click interceptor below hands cross-origin http(s)
// links to the server's POST /desktop/open-url bridge, which opens the OS
// default browser; same-origin links are left alone. Note "left alone" is
// not "working": a same-origin target="_blank" anchor (the media thumbs
// resolve to this loopback origin) is still dropped by the webview for the
// same no-new-window-handler reason — a pre-existing gap issue #179 scoped
// out, since this bridge only covers cross-origin external links.
//
// It is included by page_start ONLY when the shell marks the render as
// desktop-chrome (web.Server.SetDesktopChrome), and it additionally no-ops
// unless the <body> carries that class — a plain browser tab can never
// trigger either bridge.
(function () {
  "use strict";

  var install = function () {
    if (!document.body || !document.body.classList.contains("desktop-chrome")) {
      return;
    }

    // post delivers the drag message on whichever bridge the webview exposes:
    // window.WailsInvoke when the Wails runtime happens to be present, else
    // the raw WKWebView / WebKitGTK script-message handler the runtime itself
    // uses ("external" — registered by Wails for every page in the webview).
    var post = function (msg) {
      try {
        if (typeof window.WailsInvoke === "function") {
          window.WailsInvoke(msg);
          return;
        }
        if (
          window.webkit &&
          window.webkit.messageHandlers &&
          window.webkit.messageHandlers.external
        ) {
          window.webkit.messageHandlers.external.postMessage(msg);
        }
      } catch (err) {
        /* no webview bridge — plain browser; ignore */
      }
    };

    // Mirrors the Wails runtime's dragTest: primary button, single click, and
    // the computed --wails-draggable on the event target must be exactly
    // "drag" (interactive toolbar children set no-drag in input.css).
    //
    // Like the runtime's default (deferDragToMouseMove: true), the "drag"
    // message posts on the FIRST mousemove with the button held — not on
    // mousedown — so a plain click on empty toolbar space focuses the window
    // instead of entering a native drag session (adversarial-review fix).
    var armed = false;
    var disarm = function () {
      armed = false;
      window.removeEventListener("mousemove", onMove, true);
      window.removeEventListener("mouseup", disarm, true);
    };
    var onMove = function (e) {
      if (!armed || e.buttons !== 1) {
        disarm();
        return;
      }
      disarm();
      post("drag");
    };
    window.addEventListener("mousedown", function (e) {
      if (e.buttons !== 1 || e.detail !== 1) {
        return;
      }
      if (!(e.target instanceof Element)) {
        return;
      }
      var val = window
        .getComputedStyle(e.target)
        .getPropertyValue("--wails-draggable");
      if (!val || val.trim() !== "drag") {
        return;
      }
      armed = true;
      window.addEventListener("mousemove", onMove, true);
      window.addEventListener("mouseup", disarm, true);
    });

    // External-link opener (issue #179): a plain left-click (no modifier —
    // modified clicks keep their browser meaning) on an anchor whose resolved
    // href is http(s) on ANOTHER origin is handed to the server bridge, which
    // validates the URL again and opens the OS default browser. A failed POST
    // falls through silently: the click does nothing, exactly the pre-fix
    // behavior, and the server logs the reason.
    document.addEventListener("click", function (e) {
      if (e.defaultPrevented || e.button !== 0) {
        return;
      }
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) {
        return;
      }
      if (!(e.target instanceof Element)) {
        return;
      }
      var a = e.target.closest("a[href]");
      if (!(a instanceof HTMLAnchorElement)) {
        return;
      }
      var url;
      try {
        url = new URL(a.href); // already resolved absolute by the DOM
      } catch (err) {
        return;
      }
      if (url.protocol !== "http:" && url.protocol !== "https:") {
        return;
      }
      if (url.origin === window.location.origin) {
        // Same-origin: not ours to intercept. Plain same-origin links
        // navigate normally, but target="_blank" ones (the media thumbs)
        // are still dropped by the webview — no new-window handler — a
        // pre-existing gap out of issue #179's cross-origin scope.
        return;
      }
      e.preventDefault();
      fetch("/desktop/open-url", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: "url=" + encodeURIComponent(url.href),
      }).catch(function () {
        /* silent: link does nothing, as before the bridge existed */
      });
    });
  };

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", install);
  } else {
    install();
  }
})();
