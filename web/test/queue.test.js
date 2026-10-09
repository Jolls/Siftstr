// Tests for web/static/queue.js. Run with: node --test web/test/*.test.js
// (or through `go test ./web`, which skips them when node is not installed).
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { create, uuid } = require("../static/queue.js");

function memoryStorage(seed) {
  const m = new Map(Object.entries(seed || {}));
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
    raw: m,
  };
}

// A fake network. responder(body) returns a Response-like object, or throws to
// simulate a dropped connection.
function fakeFetch(responder) {
  const calls = [];
  const f = async (url, init) => {
    const body = JSON.parse(init.body);
    calls.push({ url, init, body });
    return responder(body, calls.length);
  };
  f.calls = calls;
  return f;
}

const okResponse = (body) => ({
  ok: true, status: 200, type: "basic",
  json: async () => ({
    results: body.actions.map((a) => ({ action_id: a.action_id, status: "applied", subject_id: a.subject_id, state: "archived" })),
  }),
});

function setup(responder, extra) {
  const storage = memoryStorage();
  const timers = [];
  const f = fakeFetch(responder);
  let n = 0;
  const q = create(Object.assign({
    userId: "u1", csrf: "tok", storage, fetch: f,
    uuid: () => "id" + ++n,
    now: () => new Date("2026-10-09T07:00:00Z"),
    setTimeout: (fn, ms) => { timers.push({ fn, ms }); return timers.length; },
    clearTimeout: () => {},
  }, extra));
  return { q, storage, f, timers };
}

const archive = (id) => ({ subject_id: id, kind: "archive" });
const key = "siftstr.queue.u1";

test("an action is sent right away with the CSRF header and leaves the queue once confirmed", async () => {
  const { q, f, storage } = setup(okResponse);
  q.enqueue(archive("itm_1"));
  await q.flush();
  assert.equal(f.calls.length, 1);
  assert.equal(f.calls[0].url, "/sync/actions");
  assert.equal(f.calls[0].init.headers["X-CSRF-Token"], "tok");
  assert.equal(f.calls[0].init.method, "POST");
  const sent = f.calls[0].body.actions[0];
  assert.deepEqual(sent, { action_id: "id1", subject_type: "item", subject_id: "itm_1", kind: "archive", at: "2026-10-09T07:00:00.000Z" });
  assert.deepEqual(q.pending(), []);
  assert.equal(storage.getItem(key), "[]");
});

test("enqueue records the action before any network answer", () => {
  const { q, storage } = setup(() => new Promise(() => {})); // never answers
  q.enqueue(archive("itm_1"));
  assert.equal(q.pending().length, 1);
  assert.equal(JSON.parse(storage.getItem(key)).length, 1);
});

test("a dropped connection keeps the actions and retries later with backoff", async () => {
  let up = false;
  const { q, f, timers } = setup((body) => {
    if (!up) throw new TypeError("network down");
    return okResponse(body);
  });
  q.enqueue(archive("itm_1"));
  q.enqueue({ subject_id: "itm_2", kind: "keep" });
  await q.flush();
  assert.equal(q.pending().length, 2);
  assert.equal(q.status, "offline");
  const first = timers.at(-1);
  assert.equal(first.ms, 2000);

  await q.flush();
  assert.equal(timers.at(-1).ms, 4000);

  up = true;
  await first.fn(); // the retry timer fires
  await q.flush();
  assert.equal(q.pending().length, 0);
  assert.equal(q.status, "idle");
  assert.equal(f.calls.at(-1).body.actions.length, 2);
});

test("the queue survives a reload", async () => {
  const storage = memoryStorage();
  const dead = fakeFetch(() => { throw new TypeError("offline"); });
  let n = 0;
  const first = create({ userId: "u1", csrf: "t", storage, fetch: dead, uuid: () => "id" + ++n, setTimeout: () => 1, clearTimeout() {} });
  first.enqueue(archive("itm_1"));
  await first.flush();

  // A new page load: a new queue over the same storage.
  const live = fakeFetch(okResponse);
  const second = create({ userId: "u1", csrf: "t2", storage, fetch: live, setTimeout: () => 1, clearTimeout() {} });
  assert.equal(second.pending().length, 1);
  await second.start({ addEventListener() {} });
  assert.equal(live.calls.length, 1);
  assert.equal(live.calls[0].body.actions[0].action_id, "id1");
  assert.equal(second.pending().length, 0);
});

test("queues are per user", () => {
  const storage = memoryStorage();
  const mk = (userId) => create({ userId, csrf: "t", storage, fetch: fakeFetch(() => new Promise(() => {})) });
  mk("u1").enqueue(archive("a"));
  assert.equal(mk("u2").pending().length, 0);
  assert.equal(mk("u1").pending().length, 1);
});

test("an ended session keeps the queue and stops retrying", async () => {
  for (const resp of [{ type: "opaqueredirect", status: 0, ok: false }, { type: "basic", status: 403, ok: false }]) {
    const { q, timers } = setup(() => resp);
    q.enqueue(archive("itm_1"));
    await q.flush();
    assert.equal(q.status, "auth");
    assert.equal(q.pending().length, 1);
    assert.equal(timers.length, 0);
  }
});

test("a server error keeps the queue and retries", async () => {
  const { q, timers } = setup(() => ({ ok: false, status: 500, type: "basic" }));
  q.enqueue(archive("itm_1"));
  await q.flush();
  assert.equal(q.status, "offline");
  assert.equal(q.pending().length, 1);
  assert.ok(timers.length > 0);
});

test("a refused batch is retried one action at a time, so valid ones survive", async () => {
  // The server refuses any batch that contains the poisoned action.
  const { q, f } = setup((body) => {
    if (body.actions.some((a) => a.subject_id === "poison")) return { ok: false, status: 400, type: "basic" };
    return okResponse(body);
  });
  q.enqueue(archive("itm_1"));
  q.enqueue(archive("poison"));
  q.enqueue(archive("itm_3"));
  for (let i = 0; i < 10 && q.pending().length; i++) await q.flush();
  assert.equal(q.pending().length, 0);
  const confirmed = f.calls.filter((c) => !c.body.actions.some((a) => a.subject_id === "poison"))
    .flatMap((c) => c.body.actions.map((a) => a.subject_id));
  assert.ok(confirmed.includes("itm_1") && confirmed.includes("itm_3"), "confirmed: " + confirmed);
});

test("a single action the server refuses is dropped so it cannot block the queue", async () => {
  const { q } = setup(() => ({ ok: false, status: 400, type: "basic" }));
  q.enqueue(archive("itm_1"));
  for (let i = 0; i < 5 && q.pending().length; i++) await q.flush();
  assert.equal(q.pending().length, 0);
});

test("a rejected action is confirmed too, and reported", async () => {
  let got;
  const { q } = setup((body) => ({
    ok: true, status: 200, type: "basic",
    json: async () => ({ results: body.actions.map((a) => ({ action_id: a.action_id, status: "rejected", reason: "already sent upstream", state: "kept" })) }),
  }), { onResults: (r) => { got = r; } });
  q.enqueue(archive("itm_1"));
  await q.flush();
  assert.equal(q.pending().length, 0);
  assert.equal(got[0].reason, "already sent upstream");
});

test("a server that confirms nothing does not make the sender spin", async () => {
  const { q, f } = setup(() => ({ ok: true, status: 200, type: "basic", json: async () => ({ results: [] }) }));
  q.enqueue(archive("itm_1"));
  await q.flush();
  assert.ok(f.calls.length <= 2, "calls: " + f.calls.length);
  assert.equal(q.pending().length, 1);
});

test("large queues go out in batches of 200", async () => {
  const { q, f } = setup(okResponse);

  for (let i = 0; i < 450; i++) q.enqueue(archive("itm_" + i)); // first flush is in flight
  await q.flush(); // joins the work
  // Let the chained flushes finish.
  for (let i = 0; i < 20 && q.pending().length; i++) await q.flush();
  assert.equal(q.pending().length, 0);
  assert.ok(f.calls.every((c) => c.body.actions.length <= 200));
});

test("a second tab's actions are not lost when this tab confirms its own", async () => {
  let injected = false;
  const { q, storage, f } = setup((body) => {
    if (!injected) { // while the first request is in flight, another tab enqueues
      injected = true;
      const list = JSON.parse(storage.getItem(key));
      list.push({ action_id: "other-tab", subject_type: "item", subject_id: "itm_9", kind: "keep", at: "x" });
      storage.setItem(key, JSON.stringify(list));
    }
    return okResponse(body);
  });
  q.enqueue(archive("itm_1"));
  await q.flush();
  // The other tab's action survived the bookkeeping and was sent next.
  const sentIDs = f.calls.flatMap((c) => c.body.actions.map((a) => a.action_id));
  assert.ok(sentIDs.includes("other-tab"), "sent: " + sentIDs);
  assert.equal(q.pending().length, 0);
});

test("undo carries its target", () => {
  const { q } = setup(() => new Promise(() => {}));
  const a = q.enqueue({ subject_id: "itm_1", kind: "undo", target_action_id: "id9" });
  assert.equal(a.target_action_id, "id9");
});

test("works without localStorage", async () => {
  const f = fakeFetch(okResponse);
  const q = create({ userId: "u1", csrf: "t", storage: null, fetch: f });
  q.enqueue(archive("itm_1"));
  assert.equal(q.pending().length, 1);
  await q.flush();
  assert.equal(q.pending().length, 0);
});

test("uuid works without crypto.randomUUID (plain HTTP pages)", () => {
  const fake = { getRandomValues: (b) => { b.fill(7); return b; } };
  const id = uuid(fake);
  assert.match(id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  assert.notEqual(uuid(), uuid());
});
