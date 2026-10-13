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
