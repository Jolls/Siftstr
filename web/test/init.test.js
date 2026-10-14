// Drives gestures.js init() against a minimal fake DOM, so a mistake in the
// code that updates a card (which the pure-function tests cannot see) shows up
// as a failing test instead of a card with no Undo button.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { init } = require("../static/gestures.js");

function el(attrs, parts) {
  const node = { dataset: attrs, hidden: false, textContent: "", parts: parts || {}, listeners: {} };
  node.querySelector = (sel) => node.parts[sel] || null;
  node.matches = (sel) => sel === "article.item";
  node.focus = () => {};
  node.closest = () => node;
  return node;
}

function page() {
  const parts = {
    "[data-badge]": { hidden: true, textContent: "", dataset: {} },
    '[data-action="undo"]': { hidden: true },
    '[data-action="promote"]': { hidden: false },
    "[data-notice]": { hidden: true, textContent: "" },
  };
  const card = el({ itemId: "itm_1", subjectType: "item", state: "light", canPromote: "true", undoAction: "", promoted: "false" }, parts);
  const handlers = {};
  const list = {
    querySelector: () => card,
    querySelectorAll: () => [card],
    addEventListener: (type, fn) => { handlers[type] = fn; },
  };
  const docHandlers = {};
  const doc = {
    body: {}, activeElement: card,
    querySelector: (sel) => (sel === ".items" ? list : null),
    addEventListener: (type, fn) => { docHandlers[type] = fn; },
  };
  const queued = [];
  const win = { siftQueue: { enqueue: (a) => { const x = Object.assign({ action_id: "act_" + (queued.length + 1) }, a); queued.push(x); return x; } } };
  init(doc, win);
  return { card, parts, handlers, docHandlers, queued };
}

test("acting on a card shows its badge and an Undo, whatever the source", () => {
  const p = page();
  p.handlers.click({ target: { closest: () => ({ dataset: { action: "archive" }, closest: () => p.card }) } });
  assert.equal(p.queued.length, 1);
  assert.equal(p.card.dataset.state, "archived");
  assert.equal(p.parts["[data-badge]"].textContent, "deleted");
  assert.equal(p.parts["[data-badge]"].hidden, false);
  assert.equal(p.parts['[data-action="undo"]'].hidden, false);
  assert.equal(p.card.dataset.undoAction, "act_1");
});

test("a promoted card that is archived reads archived, and a key undoes it", () => {
  const p = page();
  p.card.dataset.state = "deep";
  p.card.dataset.promoted = "true";
  p.docHandlers.keydown({ key: "ArrowLeft", preventDefault() {}, ctrlKey: false, metaKey: false, altKey: false });
  assert.equal(p.parts["[data-badge]"].textContent, "archived");
  p.docHandlers.keydown({ key: "z", preventDefault() {}, ctrlKey: false, metaKey: false, altKey: false });
  assert.equal(p.queued.length, 2);
  assert.equal(p.queued[1].kind, "undo");
  assert.equal(p.queued[1].target_action_id, "act_1");
  assert.equal(p.card.dataset.state, "deep");
  assert.equal(p.parts['[data-action="undo"]'].hidden, true);
});

function press(p, key) {
  p.docHandlers.keydown({ key, preventDefault() {}, ctrlKey: false, metaKey: false, altKey: false });
}

function tap(p, action) {
  p.handlers.click({ target: { closest: () => ({ dataset: { action }, closest: () => p.card }) } });
}

test("a triaged card ignores further buttons and keys until it is undone", () => {
  for (const state of ["archived", "kept", "pending_deep"]) {
    const p = page();
    p.card.dataset.state = state;
    p.card.dataset.undoAction = "act_0";
    for (const action of ["archive", "keep", "promote"]) tap(p, action);
    for (const key of ["ArrowLeft", "ArrowRight", " "]) press(p, key);
    assert.equal(p.queued.length, 0, state);
    assert.equal(p.card.dataset.state, state);
    assert.equal(p.card.dataset.undoAction, "act_0");
  }
});

test("undo puts the card, and its Promote button, back as they were", () => {
  const p = page();
  tap(p, "archive");
  assert.equal(p.card.dataset.state, "archived");
  tap(p, "undo");
  assert.equal(p.card.dataset.state, "light");
  assert.equal(p.parts['[data-action="promote"]'].hidden, false);
  assert.equal(p.parts['[data-action="undo"]'].hidden, true);

  tap(p, "promote");
  assert.equal(p.card.dataset.state, "pending_deep");
  assert.equal(p.parts['[data-action="promote"]'].hidden, true);
  tap(p, "undo");
  assert.equal(p.card.dataset.state, "light");
  assert.equal(p.parts['[data-action="promote"]'].hidden, false);
});

test("badge color hook follows the word", () => {
  const p = page();
  const badgeEl = p.parts["[data-badge]"];
  tap(p, "archive");
  assert.equal(badgeEl.dataset.badge, "deleted");
  press(p, "z");
  assert.equal(badgeEl.dataset.badge, "");

  const q = page();
  q.card.dataset.state = "deep";
  q.card.dataset.promoted = "true";
  tap(q, "archive");
  assert.equal(q.parts["[data-badge]"].dataset.badge, "archived");

  const k = page();
  tap(k, "keep");
  assert.equal(k.parts["[data-badge]"].dataset.badge, "kept");

  const r = page();
  tap(r, "promote");
  assert.equal(r.parts["[data-badge]"].dataset.badge, "promoted");
});

// Two cards, a phone-width window and scroll recorders, for the scroll tests.
// Card 1 sits at the bottom edge 800; once it is actioned, card 2 sits at 500.
function scrolly(opts) {
  const o = Object.assign({ phone: false, reduced: false }, opts);
  function card(id, bottom) {
    const parts = {
      "[data-badge]": { hidden: true, textContent: "", dataset: {} },
      '[data-action="undo"]': { hidden: true },
      '[data-action="promote"]': { hidden: false },
      "[data-notice]": { hidden: true, textContent: "" },
    };
    const c = el({ itemId: id, subjectType: "item", state: "light", canPromote: "true", undoAction: "", promoted: "false" }, parts);
    c.parts = parts;
    c.bottom = bottom;
    c.getBoundingClientRect = () => ({ bottom: c.bottom });
    c.focused = [];
    c.focus = (a) => c.focused.push(a || {});
    c.scrolled = [];
    c.scrollIntoView = (a) => c.scrolled.push(a);
    return c;
  }
  const c1 = card("itm_1", 800), c2 = card("itm_2", 500);
  c1.closest = () => c1;
  c2.closest = () => c2;
  const handlers = {};
  const list = {
    dataset: { flow: "up" },
    querySelector: () => c1,
    querySelectorAll: () => [c1, c2],
    addEventListener: (type, fn) => { handlers[type] = fn; },
  };
  const docHandlers = {};
  const doc = {
    body: {}, activeElement: c1,
    querySelector: (sel) => (sel === ".items" ? list : null),
    addEventListener: (type, fn) => { docHandlers[type] = fn; },
  };
  const queued = [];
  const scrollBy = [];
  const win = {
    siftQueue: { enqueue: (a) => { const x = Object.assign({ action_id: "act_" + (queued.length + 1) }, a); queued.push(x); return x; } },
    scrollBy: (a) => scrollBy.push(a),
  };
  if (o.phone || o.reduced) {
    win.matchMedia = (q) => ({ matches: q.indexOf("max-width") >= 0 ? o.phone : o.reduced });
  }
  init(doc, win);
  c1.scrolled.length = 0; c2.scrolled.length = 0; c1.focused.length = 0; c2.focused.length = 0;
  return { c1, c2, list, handlers, docHandlers, queued, scrollBy, win };
}

function key2(p, key) {
  p.docHandlers.keydown({ key, preventDefault() {}, ctrlKey: false, metaKey: false, altKey: false });
}
function tap2(p, card, action) {
  p.handlers.click({ target: { closest: () => ({ dataset: { action }, closest: () => card }) } });
}

// Characterization: today's behavior outside the bottom-up view.
test("top-down: a key action focuses the next card and scrolls it into view, nearest", () => {
  const p = scrolly();
  key2(p, "ArrowLeft");
  assert.equal(p.c2.focused.length, 1);
  assert.deepEqual(p.c2.scrolled, [{ block: "nearest", behavior: "smooth" }]);
  assert.equal(p.scrollBy.length, 0);
});

test("top-down: a button action does not scroll or move focus", () => {
  const p = scrolly();
  tap2(p, p.c1, "archive");
  assert.equal(p.scrollBy.length, 0);
  assert.equal(p.c2.scrolled.length, 0);
  assert.equal(p.c2.focused.length, 0);
});

test("an action that does not apply scrolls nothing", () => {
  const p = scrolly({ phone: true });
  p.c1.dataset.state = "archived";
  tap2(p, p.c1, "archive");
  assert.equal(p.queued.length, 0);
  assert.equal(p.scrollBy.length, 0);
});

test("bottom-up: the last card to sift scrolls nothing", () => {
  const p = scrolly({ phone: true });
  p.c2.dataset.state = "kept";
  tap2(p, p.c1, "archive");
  assert.equal(p.queued.length, 1);
  assert.equal(p.scrollBy.length, 0);
});

// Red: bottom-up scrolls the next card into the actioned card's place.
test("bottom-up: a button action scrolls the next card's bottom to the old bottom", () => {
  const p = scrolly({ phone: true });
  tap2(p, p.c1, "archive");
  assert.equal(p.scrollBy.length, 1);
  assert.equal(p.scrollBy[0].top, -300);
  assert.deepEqual(p.c2.focused, [{ preventScroll: true }]);
});

test("bottom-up: a swipe scrolls the same way", () => {
  const p = scrolly({ phone: true });
  p.handlers.pointerdown({ pointerType: "touch", clientX: 200, clientY: 100, target: { closest: (s) => (s === "article.item" ? p.c1 : null) } });
  p.handlers.pointerup({ clientX: 110, clientY: 100 });
  assert.equal(p.queued.length, 1);
  assert.equal(p.scrollBy.length, 1);
  assert.equal(p.scrollBy[0].top, -300);
});

test("bottom-up: a key action scrolls the same way and focuses the next card", () => {
  const p = scrolly({ phone: true });
  key2(p, " ");
  assert.equal(p.scrollBy.length, 1);
  assert.equal(p.scrollBy[0].top, -300);
  assert.equal(p.c2.focused.length, 1);
  assert.equal(p.c2.scrolled.length, 0);
});

test("bottom-up: the scroll is smooth, or instant when reduced motion is asked for", () => {
  const a = scrolly({ phone: true });
  tap2(a, a.c1, "archive");
  assert.equal(a.scrollBy[0].behavior, "smooth");
  const b = scrolly({ phone: true, reduced: true });
  tap2(b, b.c1, "archive");
  assert.equal(b.scrollBy[0].behavior, "auto");
});

test("bottom-up: the actioned card stays with its badge and undo does not scroll", () => {
  const p = scrolly({ phone: true });
  tap2(p, p.c1, "keep");
  assert.equal(p.list.querySelectorAll().length, 2);
  assert.equal(p.c1.parts["[data-badge]"].textContent, "kept");
  assert.equal(p.scrollBy.length, 1);
  tap2(p, p.c1, "undo");
  assert.equal(p.c1.dataset.state, "light");
  assert.equal(p.scrollBy.length, 1);
});
