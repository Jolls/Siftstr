// Triage gestures, keys and buttons (ARCHITECTURE.md sections 5, 7 and 11).
//
// Swipe left or Left archives, swipe right or Right promotes, tap or Space
// keeps. Every route ends in the same call, act(), which updates the badge
// at once and puts the action in the page queue (queue.js). Nothing here
// makes a request or waits on the network, and a card is never removed.
(function (root, factory) {
  var api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  else {
    root.SiftGestures = api;
    if (root.document) api.init(root.document, root);
  }
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";

  var BADGES = { archived: "archived", kept: "kept", pending_deep: "promoted" }; // same words as server.go
  var SWIPE_MIN = 60; // px
  var TAP_MAX_MOVE = 10; // px
  var TAP_MAX_MS = 400;

  // next is the state an action leads to, or null if it does not apply. It
  // mirrors the server's rules; the server stays the authority and corrects
  // the page if they ever disagree.
  function next(state, kind, canPromote) {
    if (state !== "light" && state !== "deep") return null;
    if (kind === "archive") return "archived";
    if (kind === "keep") return "kept";
    if (kind === "promote") {
      if (!canPromote) return null;
      return state === "deep" ? "kept" : "pending_deep";
    }
    return null;
  }

  // swipe turns a finished touch into an action kind, or null. A touch that
  // began on a link or button can swipe but never counts as a tap, so those
  // keep their own taps.
  function swipe(dx, dy, ms, onControl) {
    var ax = Math.abs(dx), ay = Math.abs(dy);
    if (ax >= SWIPE_MIN && ax > ay * 1.5) return dx < 0 ? "archive" : "promote";
    if (!onControl && ax < TAP_MAX_MOVE && ay < TAP_MAX_MOVE && ms <= TAP_MAX_MS) return "keep";
    return null;
  }

  var KEYS = { ArrowLeft: "archive", ArrowRight: "promote", " ": "keep", u: "undo", z: "undo" };

  // card is the in-page record of one article: what act() and the server
  // results change.
  function readCard(el) {
    return {
      id: el.dataset.itemId,
      type: el.dataset.subjectType || "item",
      state: el.dataset.state,
      canPromote: el.dataset.canPromote === "true",
      undoID: el.dataset.undoAction || "",
      prev: el.dataset.prevState || "",
    };
  }

  // reconcile says what a card should look like after the server's result for
  // it: the server's state, and whether an undo is still possible.
  function reconcile(card, result) {
    var out = { state: result.state, undoID: card.undoID, prev: card.prev, notice: "" };
    if (out.state === "light" || out.state === "deep") {
      out.undoID = "";
      out.prev = "";
    } else if (result.status === "rejected") {
      out.undoID = ""; // whatever it was, it can no longer be taken back
    }
    if (result.status === "rejected" && result.reason) out.notice = result.reason;
    return out;
  }

  function init(doc, win) {
    var list = doc.querySelector(".items");
    if (!list) return;

    function cards() { return Array.prototype.slice.call(list.querySelectorAll("article.item")); }

    function render(el) {
      var c = readCard(el);
      var badge = el.querySelector("[data-badge]");
      var text = BADGES[c.state] || "";
      badge.textContent = text;
      badge.hidden = text === "";
      var undo = el.querySelector('[data-action="undo"]');
      if (undo) undo.hidden = c.undoID === "";
      var promote = el.querySelector('[data-action="promote"]');
      if (promote) promote.hidden = c.state !== "light";
    }

    function set(el, v) {
      el.dataset.state = v.state;
      el.dataset.undoAction = v.undoID;
      el.dataset.prevState = v.prev;
      var notice = el.querySelector("[data-notice]");
      if (notice) { notice.textContent = v.notice || ""; notice.hidden = !v.notice; }
      render(el);
    }

    // act records one action on a card and shows its effect immediately.
    function act(el, kind) {
      var q = win.siftQueue;
      var c = readCard(el);
      if (!q) return;
      if (kind === "undo") {
        if (!c.undoID) return;
        q.enqueue({ subject_type: c.type, subject_id: c.id, kind: "undo", target_action_id: c.undoID });
        set(el, { state: c.prev || "light", undoID: "", prev: "", notice: "" });
        return;
      }
      var to = next(c.state, kind, c.canPromote);
      if (!to) return;
      var a = q.enqueue({ subject_type: c.type, subject_id: c.id, kind: kind });
      set(el, { state: to, undoID: a.action_id, prev: c.state, notice: "" });
    }

    // Buttons.
    list.addEventListener("click", function (e) {
      var btn = e.target.closest && e.target.closest("[data-action]");
      if (!btn) return;
      var el = btn.closest("article.item");
      if (el) act(el, btn.dataset.action);
    });

    // Keys act on the focused card only, so Space and arrows keep their usual
    // job on links, buttons and form fields.
    function focusCard(el) {
      if (!el) return;
      el.focus();
      if (el.scrollIntoView) el.scrollIntoView({ block: "nearest" });
    }
    doc.addEventListener("keydown", function (e) {
      if (e.ctrlKey || e.metaKey || e.altKey) return;
      var all = cards();
      var cur = doc.activeElement && doc.activeElement.matches && doc.activeElement.matches("article.item") ? doc.activeElement : null;
      if (e.key === "j" || e.key === "ArrowDown" || e.key === "k" || e.key === "ArrowUp") {
        if (!cur && doc.activeElement !== doc.body) return; // let controls keep their keys
        var step = e.key === "j" || e.key === "ArrowDown" ? 1 : -1;
        var i = cur ? all.indexOf(cur) + step : 0;
        if (all[i]) { e.preventDefault(); focusCard(all[i]); }
        return;
      }
      var kind = KEYS[e.key];
      if (!kind || !cur) return;
      e.preventDefault();
      act(cur, kind);
      if (kind !== "undo") focusCard(all[all.indexOf(cur) + 1]); // on to the next card
    });

    // Touch: swipe left/right, tap to keep. Links and buttons keep their own taps.
    var start = null;
    list.addEventListener("pointerdown", function (e) {
      if (e.pointerType !== "touch") return;
      start = { x: e.clientX, y: e.clientY, t: Date.now(), el: e.target.closest("article.item"), onControl: !!e.target.closest("a, button") };
    });
    list.addEventListener("pointerup", function (e) {
      var s = start;
      start = null;
      if (!s || !s.el) return;
      var kind = swipe(e.clientX - s.x, e.clientY - s.y, Date.now() - s.t, s.onControl);
      if (kind) act(s.el, kind);
    });
    list.addEventListener("pointercancel", function () { start = null; });

    // The server's view wins: reconcile every card it reports on.
    doc.addEventListener("siftstr:results", function (e) {
      (e.detail || []).forEach(function (r) {
        var el = list.querySelector('[data-item-id="' + (win.CSS && win.CSS.escape ? win.CSS.escape(r.subject_id) : r.subject_id) + '"]');
        if (!el || r.state === undefined || r.state === "") return;
        set(el, reconcile(readCard(el), r));
      });
    });

    cards().forEach(render);
  }

  return { next: next, swipe: swipe, reconcile: reconcile, init: init, KEYS: KEYS };
});
