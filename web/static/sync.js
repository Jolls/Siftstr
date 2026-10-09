// Starts the page's action queue and shows its state. gestures.js and the
// buttons enqueue through window.siftQueue; this file only wires it up.
(function () {
  "use strict";
  var body = document.body;
  var userID = body.dataset.userId;
  if (!userID || !window.SiftQueue) return;

  function actions(n) { return n + " action" + (n === 1 ? "" : "s"); }

  var indicator = document.querySelector("[data-sync-status]");

  function show(q) {
    if (!indicator) return;
    var n = q.pending().length;
    var text = "";
    if (n > 0) {
      if (q.status === "auth") text = "Sign in again to send " + actions(n);
      else if (q.status === "offline") text = "Offline: " + n + " waiting to send";
      else text = n + " sending";
    }
    indicator.textContent = text;
    indicator.hidden = text === "";
    indicator.dataset.state = q.status;
  }

  var q = window.SiftQueue.create({
    userId: userID,
    csrf: body.dataset.csrf,
    onChange: show,
    onResults: function (results) {
      // Reconcile badges with the server's version of each subject.
      document.dispatchEvent(new CustomEvent("siftstr:results", { detail: results }));
    },
  });
  window.siftQueue = q;
  show(q);
  q.start(window);

  // Logging out would leave unsent actions waiting for the next login.
  var logout = document.querySelector("form[data-logout]");
  if (logout) {
    logout.addEventListener("submit", function (e) {
      var n = q.pending().length;
      if (n > 0 && !window.confirm(actions(n) + (n === 1 ? " has" : " have") + " not been sent yet. Sign out anyway?")) {
        e.preventDefault();
      }
    });
  }
})();
