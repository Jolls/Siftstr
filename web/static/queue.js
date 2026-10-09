// The page's action queue and sender (ARCHITECTURE.md section 7).
//
// Every triage gesture, key and button enqueues an action here. The queue is
// mirrored to localStorage so a reload, or a tab killed by the OS, loses
// nothing. The sender posts the queue to /sync/actions right away and keeps
// the actions until the server confirms them, retrying when the connection
// returns. Actions are idempotent by action_id, so sending twice is harmless.
// Nothing in the UI waits on the network.
(function (root, factory) {
  var api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  else root.SiftQueue = api;
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";

  var BATCH = 200; // the server accepts 500; stay well inside
  var RETRY_MIN = 2000;
  var RETRY_MAX = 60000;
  var POLL = 30000;

  // crypto.randomUUID needs a secure context, and many installs are plain
  // HTTP on a LAN, so build a v4 UUID from getRandomValues, which does not.
  function uuid(cryptoObj) {
    var c = cryptoObj || crypto;
    if (c.randomUUID) return c.randomUUID();
    var b = c.getRandomValues(new Uint8Array(16));
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    var h = Array.prototype.map.call(b, function (x) { return (x + 0x100).toString(16).slice(1); }).join("");
    return h.slice(0, 8) + "-" + h.slice(8, 12) + "-" + h.slice(12, 16) + "-" + h.slice(16, 20) + "-" + h.slice(20);
  }

  // create returns a queue for one user. Options: userId, csrf, and for tests
  // storage, fetch, now, uuid, setTimeout, clearTimeout, url, onChange,
  // onResults.
  function create(o) {
    var storage = o.storage !== undefined ? o.storage : safeLocalStorage();
    var doFetch = o.fetch || (typeof fetch !== "undefined" ? fetch.bind(globalThis) : null);
    var now = o.now || function () { return new Date(); };
    var newID = o.uuid || function () { return uuid(); };
    var setT = o.setTimeout || setTimeout;
    var clearT = o.clearTimeout || clearTimeout;
    var url = o.url || "/sync/actions";
    var key = "siftstr.queue." + o.userId;

    var q = { status: "idle" }; // idle | sending | offline | auth
    var memory = []; // used when storage is unavailable
    var sending = false;
    var again = false;
    var current = Promise.resolve(); // the send in flight
    var batchSize = BATCH; // drops to 1 to isolate a batch the server refuses
    var retryMs = RETRY_MIN;
    var timer = null;

    function read() {
      if (!storage) return memory.slice();
      try {
        var v = JSON.parse(storage.getItem(key) || "[]");
        return Array.isArray(v) ? v : [];
      } catch (e) {
        return [];
      }
    }
    function write(list) {
      if (!storage) { memory = list.slice(); return; }
      try { storage.setItem(key, JSON.stringify(list)); } catch (e) { memory = list.slice(); }
    }
    // mutate re-reads the stored queue first, so a second tab's actions are
    // not overwritten by this tab's bookkeeping.
    function mutate(fn) {
      var list = read();
      fn(list);
      write(list);
      changed();
    }
    function changed() {
      if (o.onChange) o.onChange(q);
    }

    q.pending = function () { return read(); };

    q.enqueue = function (a) {
      var action = {
        action_id: newID(),
        subject_type: a.subject_type || "item",
        subject_id: a.subject_id,
        kind: a.kind,
        at: now().toISOString(),
      };
      if (a.target_action_id) action.target_action_id = a.target_action_id;
      mutate(function (list) { list.push(action); });
      q.flush();
      return action;
    };

    function setStatus(s) {
      if (q.status === s) return;
      q.status = s;
      changed();
    }

    function schedule() {
      if (timer !== null) clearT(timer);
      timer = setT(function () { timer = null; q.flush(); }, retryMs);
      retryMs = Math.min(retryMs * 2, RETRY_MAX);
    }

    // flush sends the queue. It returns a promise that resolves when this
    // attempt is over; failures are handled here and never reject.
    q.flush = function () {
      if (sending) { again = true; return current; }
      var batch = read().slice(0, batchSize);
      if (!batch.length) { batchSize = BATCH; if (q.status !== "idle") setStatus("idle"); return Promise.resolve(); }
      sending = true;
      var progressed = false; // the server confirmed or refused something
      setStatus("sending");
      current = doFetch(url, {
        method: "POST",
        credentials: "same-origin",
        redirect: "manual", // a redirect means the session ended
        headers: { "Content-Type": "application/json", "X-CSRF-Token": o.csrf },
        body: JSON.stringify({ actions: batch }),
      }).then(function (resp) {
        if (resp.type === "opaqueredirect" || resp.status === 401 || resp.status === 403) {
          setStatus("auth"); // keep everything; it goes out after the next login
          return;
        }
        if (resp.status === 400 || resp.status === 413) {
          // The server will never accept this batch. One bad action must not
          // take the valid ones with it, so send them one at a time; a single
          // action it still refuses is dropped, since retrying it forever
          // would block the queue.
          if (batch.length > 1) batchSize = 1;
          else drop([batch[0].action_id]);
          progressed = true;
          return;
        }
        if (!resp.ok) throw new Error("status " + resp.status);
        return resp.json().then(function (body) {
          var results = (body && body.results) || [];
          var confirmed = results.map(function (r) { return r.action_id; });
          drop(confirmed);
          progressed = confirmed.length > 0;
          retryMs = RETRY_MIN;
          if (o.onResults) o.onResults(results);
        });
      }).catch(function () {
        setStatus("offline");
        schedule();
      }).then(function () {
        sending = false;
        if (q.status === "sending") setStatus("idle");
        // Send the rest only if that round got somewhere, so a server that
        // keeps answering with nothing cannot make this loop spin.
        var more = progressed && read().length > 0;
        var retry = again;
        again = false;
        if (q.status === "offline" || q.status === "auth") return; // the retry timer or a login takes it from here
        if (more || retry) return q.flush();
      });
      return current;
    };

    function drop(ids) {
      var gone = {};
      ids.forEach(function (id) { gone[id] = true; });
      mutate(function (list) {
        for (var i = list.length - 1; i >= 0; i--) if (gone[list[i].action_id]) list.splice(i, 1);
      });
    }

    // start sends what is waiting and keeps trying: on the browser's online
    // event, on an interval, and (by being called) on every page load.
    q.start = function (win) {
      win = win || (typeof window !== "undefined" ? window : null);
      if (win && win.addEventListener) {
        win.addEventListener("online", function () { retryMs = RETRY_MIN; q.flush(); });
      }
      setT(function tick() { q.flush(); setT(tick, POLL); }, POLL);
      return q.flush();
    };

    return q;
  }

  function safeLocalStorage() {
    try {
      var s = typeof localStorage !== "undefined" ? localStorage : null;
      if (s) { s.setItem("siftstr.t", "1"); s.removeItem("siftstr.t"); }
      return s;
    } catch (e) {
      return null;
    }
  }

  return { create: create, uuid: uuid };
});
