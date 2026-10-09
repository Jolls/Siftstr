// Tests for the decision logic in web/static/gestures.js.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { next, swipe, undoTarget, reconcile, KEYS } = require("../static/gestures.js");

test("archive and keep apply to light and deep cards", () => {
  for (const s of ["light", "deep"]) {
    assert.equal(next(s, "archive", false), "archived");
    assert.equal(next(s, "keep", false), "kept");
  }
});

test("promote moves light to pending_deep, acts as keep on deep, and is off where disallowed", () => {
  assert.equal(next("light", "promote", true), "pending_deep");
  assert.equal(next("deep", "promote", true), "kept");
  assert.equal(next("light", "promote", false), null);
});

test("actioned and waiting cards take no new action", () => {
  for (const s of ["archived", "kept", "pending_deep", "pending_light", "expired"]) {
    for (const k of ["archive", "keep", "promote", "undo"]) assert.equal(next(s, k, true), null, s + " " + k);
  }
});

test("swipes: left archives, right promotes, mostly-vertical movement does nothing", () => {
  assert.equal(swipe(-90, 5, 200, false), "archive");
  assert.equal(swipe(90, -5, 200, false), "promote");
  assert.equal(swipe(-40, 0, 200, false), null); // too short
  assert.equal(swipe(80, 100, 200, false), null); // a scroll
});

test("a short, quick touch is a tap that keeps, unless it began on a control", () => {
  assert.equal(swipe(2, 3, 150, false), "keep");
  assert.equal(swipe(2, 3, 150, true), null);
  assert.equal(swipe(2, 3, 900, false), null); // a long press
  assert.equal(swipe(20, 3, 150, false), null); // moved too far for a tap
  assert.equal(swipe(-90, 0, 200, true), "archive"); // a swipe from a control still counts
});

test("key bindings", () => {
  assert.deepEqual([KEYS.ArrowLeft, KEYS.ArrowRight, KEYS[" "], KEYS.u], ["archive", "promote", "keep", "undo"]);
});

const card = (o) => Object.assign({ id: "itm_1", type: "item", state: "archived", canPromote: true, undoID: "a1", prev: "light" }, o);

test("reconcile: an applied action keeps its undo", () => {
  const r = reconcile(card(), { status: "applied", state: "archived" });
  assert.deepEqual(r, { state: "archived", undoID: "a1", prev: "light", notice: "" });
});

test("reconcile: a card the server says is untriaged has nothing to undo", () => {
  const r = reconcile(card(), { status: "applied", state: "light" });
  assert.equal(r.state, "light");
  assert.equal(r.undoID, "");
});

test("reconcile: a rejection shows the server's state and reason, and removes undo", () => {
  const r = reconcile(card({ state: "archived" }), { status: "rejected", state: "kept", reason: "already sent upstream" });
  assert.deepEqual(r, { state: "kept", undoID: "", prev: "light", notice: "already sent upstream" });
});

test("reconcile: a replay is not an error", () => {
  const r = reconcile(card(), { status: "duplicate", state: "archived" });
  assert.equal(r.undoID, "a1");
  assert.equal(r.notice, "");
});

test("undo goes to the focused card if it can be undone, else the latest actioned one", () => {
  const can = (c) => c.undo;
  const a = { id: "a", undo: true }, b = { id: "b", undo: true }, c = { id: "c", undo: false };
  assert.equal(undoTarget(a, [b], can), a);
  // Acting moved focus on to c, which has nothing to undo: undo the last action.
  assert.equal(undoTarget(c, [a, b], can), b);
  assert.equal(undoTarget(null, [a, b], can), b);
  // Skips cards whose undo is gone (sent, or already undone).
  b.undo = false;
  assert.equal(undoTarget(c, [a, b], can), a);
  a.undo = false;
  assert.equal(undoTarget(c, [a, b], can), null);
  assert.equal(undoTarget(null, [], can), null);
});
