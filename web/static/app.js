// Belay UI: a styled confirm modal (with optional changelog link) + client-side service search.
(function () {
  function modal(question, onOK, changelog, okLabel) {
    const ov = document.createElement("div");
    ov.className = "modal-overlay";
    ov.innerHTML =
      '<div class="modal" role="dialog" aria-modal="true">' +
      '<p class="modal-q"></p>' +
      '<div class="modal-actions">' +
      '<button class="btn ghost" data-cancel>Cancel</button>' +
      '<button class="btn" data-ok></button>' +
      "</div></div>";
    ov.querySelector(".modal-q").textContent = question;
    ov.querySelector("[data-ok]").textContent = okLabel || "Confirm";
    if (changelog) {
      const a = document.createElement("a"); // built via DOM (no innerHTML injection)
      a.className = "modal-link";
      a.href = changelog;
      a.target = "_blank";
      a.rel = "noopener";
      a.textContent = "View changelog ↗";
      ov.querySelector(".modal-q").after(a);
    }
    document.body.appendChild(ov);
    const close = () => ov.remove();
    ov.addEventListener("click", (e) => { if (e.target === ov) close(); });
    function esc(e) { if (e.key === "Escape") { close(); document.removeEventListener("keydown", esc); } }
    document.addEventListener("keydown", esc);
    ov.querySelector("[data-cancel]").onclick = close;
    ov.querySelector("[data-ok]").onclick = () => { close(); onOK(); };
    ov.querySelector("[data-ok]").focus();
  }

  // htmx elements with hx-confirm route through our modal (with changelog link + custom OK label).
  document.addEventListener("htmx:confirm", function (e) {
    if (!e.detail.question) return;
    e.preventDefault();
    const el = e.detail.elt;
    const cl = el && el.getAttribute("data-changelog");
    const ok = (el && el.getAttribute("data-ok-label")) || "Confirm";
    modal(e.detail.question, () => e.detail.issueRequest(true), cl, ok);
  });

  // Plain forms (Update all, self-update) confirm via the same modal.
  window.belayConfirm = function (question, form, okLabel) {
    modal(question, () => form.submit(), null, okLabel || "Confirm");
    return false;
  };

  // Review page: show/hide the per-version changelog blocks.
  window.belayToggleChangelogs = function (on) {
    document.querySelectorAll(".changelogs").forEach(function (d) { d.hidden = !on; });
  };

  // Review page: called after each service's check returns. Tracks progress, hides up-to-date rows,
  // and unlocks "Update all" once every service has been checked.
  window.belayReviewChecked = function (el) {
    var list = document.getElementById("review-list");
    if (!list) return;
    var item = el.closest(".review-item");
    if (item) item.setAttribute("data-done", "1");
    var mark = el.querySelector("[data-updatable]");
    var updatable = mark && mark.getAttribute("data-updatable") === "1";
    if (item && !updatable) item.style.display = "none"; // hide up-to-date / errored services
    var total = parseInt(list.getAttribute("data-total") || "0", 10);
    var done = list.querySelectorAll('.review-item[data-done="1"]').length;
    var prog = document.getElementById("rev-progress");
    if (prog) prog.textContent = done + "/" + total;
    if (done >= total) {
      var n = list.querySelectorAll('.review-item [data-updatable="1"]').length;
      var btn = document.getElementById("update-all-btn");
      var hint = document.getElementById("rev-hint");
      if (btn) {
        if (n > 0) { btn.disabled = false; btn.textContent = "Update all " + n; }
        else { btn.textContent = "Nothing to update"; btn.classList.add("ghost"); }
      }
      if (hint) hint.textContent = n > 0 ? (n + " service(s) with a newer version.") : "Everything is up to date. 🎉";
    }
  };

  // Remember which host/project groups are collapsed (per group key, in localStorage).
  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll("details[data-grp]").forEach(function (d) {
      const k = "belay-grp:" + d.getAttribute("data-grp");
      const v = localStorage.getItem(k);
      if (v === "0") d.open = false;
      else if (v === "1") d.open = true;
      d.addEventListener("toggle", function () { localStorage.setItem(k, d.open ? "1" : "0"); });
    });
  });

  // Filter service rows on the Updates tab.
  window.belayFilter = function (q) {
    q = q.toLowerCase();
    document.querySelectorAll('tr[id^="row-"]').forEach(function (tr) {
      tr.style.display = tr.textContent.toLowerCase().indexOf(q) >= 0 ? "" : "none";
    });
  };

  // ---- Details popout (logs + full error) -------------------------------------------------
  // The lists carry only what fits on a card; the bulky parts are fetched per record. That keeps
  // every poll small, and it means a 2000-line log no longer has to live inside a list row.
  window.belayRecord = function (id) {
    fetch("/record?id=" + encodeURIComponent(id), { credentials: "same-origin" })
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(showRecord)
      .catch(function (err) {
        showRecord({ service: "Record " + id, error: "Could not load details: " + err.message });
      });
  };

  function logSection(title, text, cls) {
    const sec = document.createElement("div");
    sec.className = "log-sec";
    const h = document.createElement("h4");
    h.textContent = title;
    const pre = document.createElement("pre");
    if (cls) pre.className = cls;
    pre.textContent = text; // textContent, never innerHTML: log output is untrusted
    sec.append(h, pre);
    return sec;
  }

  function showRecord(rec) {
    const ov = document.createElement("div");
    ov.className = "modal-overlay";
    ov.innerHTML =
      '<div class="modal modal-lg" role="dialog" aria-modal="true">' +
      '<div class="log-head"><b data-title></b><span class="mono muted" data-sub></span>' +
      '<button class="x" data-close aria-label="Close">✕</button></div>' +
      '<div data-body></div>' +
      '<div class="modal-actions"><button class="btn ghost" data-copy>Copy</button>' +
      '<button class="btn" data-close>Close</button></div></div>';

    ov.querySelector("[data-title]").textContent = rec.service || "Details";
    const sub = [];
    if (rec.from && rec.to) sub.push((rec.repo ? rec.repo + " " : "") + rec.from + " → " + rec.to);
    if (rec.when) sub.push(rec.when);
    ov.querySelector("[data-sub]").textContent = sub.join(" · ");

    const body = ov.querySelector("[data-body]");
    if (rec.error) body.appendChild(logSection("Error", rec.error, "err-text"));
    if (rec.logs) body.appendChild(logSection("Container logs", rec.logs, "log-text"));
    if (!rec.error && !rec.logs) {
      const p = document.createElement("p");
      p.className = "log-empty";
      p.textContent = "Nothing was captured for this attempt.";
      body.appendChild(p);
    }

    document.body.appendChild(ov);
    const close = function () {
      ov.remove();
      document.removeEventListener("keydown", esc);
    };
    function esc(e) { if (e.key === "Escape") close(); }
    document.addEventListener("keydown", esc);
    ov.addEventListener("click", function (e) { if (e.target === ov) close(); });
    ov.querySelectorAll("[data-close]").forEach(function (b) { b.onclick = close; });
    ov.querySelector("[data-copy]").onclick = function (e) {
      const text = [rec.error, rec.logs].filter(Boolean).join("\n\n");
      if (!navigator.clipboard) return;
      navigator.clipboard.writeText(text).then(function () {
        e.target.textContent = "Copied";
        setTimeout(function () { e.target.textContent = "Copy"; }, 1200);
      });
    };
    // Container logs are read from the end — land on the newest line. The error is NOT scrolled:
    // its first line is the one that says what went wrong.
    ov.querySelectorAll("pre.log-text").forEach(function (pre) { pre.scrollTop = pre.scrollHeight; });
    ov.querySelector("[data-close]").focus();
  }

  // ---- Keeping background polls out of the user's way -------------------------------------
  // Every polled list is marked data-poll. Three rules, each fixing a distinct kind of churn:
  //   1. don't even ask while a dialog is open or text is selected inside the list;
  //   2. don't swap when the server says nothing changed (data-rev, or an identical response);
  //   3. when we do swap, restore the open/scroll state of any live log box.
  function isPolled(el) { return !!(el && el.hasAttribute && el.hasAttribute("data-poll")); }

  function selectionInside(el) {
    const sel = window.getSelection && window.getSelection();
    if (!sel || sel.isCollapsed || !sel.rangeCount) return false;
    return el.contains(sel.getRangeAt(0).commonAncestorContainer);
  }

  // A polled request answered by a DIFFERENT ORIGIN, or refused outright, means Belay's session is
  // gone and the identity provider replied instead. Polling through that is how one forgotten tab
  // becomes a redirect storm: every poll is a full SSO round trip, forever, each one minting login
  // state in the browser until the cookie jar is too big to send and the IdP starts rejecting the
  // request. So the poll stops at the first sign the session is dead, and says so.
  var pollingStopped = false;
  var pollFailures = 0;

  function stopPolling(reason) {
    if (pollingStopped) return;
    pollingStopped = true;
    const bar = document.createElement("div");
    bar.className = "session-lost";
    bar.innerHTML = '<span></span><button class="btn" data-reload>Reload</button>';
    bar.querySelector("span").textContent = reason;
    bar.querySelector("[data-reload]").onclick = function () { location.reload(); };
    document.body.appendChild(bar);
  }

  document.addEventListener("htmx:beforeRequest", function (e) {
    const t = e.detail.target;
    if (!isPolled(t)) return;
    if (pollingStopped) { e.preventDefault(); return; }
    // A refresh mid-selection wipes the selection; a refresh behind a modal is invisible anyway.
    if (document.querySelector(".modal-overlay") || selectionInside(t)) { e.preventDefault(); return; }
    // The Activity tray is the most frequent poll by far. While it is closed there is nothing to
    // repaint, so it drops to a heartbeat instead of running at its open-tray rate.
    if (t.id === "activity-body" && trayHidden() && Date.now() - lastTrayPoll < 15000) {
      e.preventDefault();
      return;
    }
    if (t.id === "activity-body") lastTrayPoll = Date.now();
  });

  var lastTrayPoll = 0;
  function trayHidden() {
    const tray = document.getElementById("activity-tray");
    return !tray || tray.classList.contains("hidden");
  }

  document.addEventListener("htmx:afterRequest", function (e) {
    if (!isPolled(e.detail.target)) return;
    const xhr = e.detail.xhr;
    // status 0 = the response was blocked, which is what a cross-origin redirect to the IdP looks
    // like from an XHR. 401/403 = still us, but no longer authorised.
    const dead = !xhr || xhr.status === 0 || xhr.status === 401 || xhr.status === 403;
    let offsite = false;
    try {
      offsite = !!(xhr && xhr.responseURL) && new URL(xhr.responseURL).origin !== location.origin;
    } catch (_) { /* opaque response */ }
    if (!dead && !offsite) { pollFailures = 0; return; }
    // One blip is a blip; two in a row is a dead session. Requiring two avoids stopping on a
    // single dropped request during a deploy.
    if (++pollFailures >= 2) {
      stopPolling(offsite
        ? "Your session expired and Belay stopped refreshing. Reload to sign in again."
        : "Belay lost its connection and stopped refreshing.");
    }
  });

  var lastResponse = {};
  document.addEventListener("htmx:beforeSwap", function (e) {
    const t = e.detail.target;
    if (!isPolled(t)) return;
    const resp = e.detail.serverResponse;
    if (t.hasAttribute("data-rev")) {
      // The list stamps a fingerprint of its rows; equal fingerprint means an identical render.
      const doc = new DOMParser().parseFromString(resp || "", "text/html");
      const frag = t.id ? doc.getElementById(t.id) : null;
      if (frag && frag.getAttribute("data-rev") === t.getAttribute("data-rev")) {
        e.detail.shouldSwap = false;
        return;
      }
    } else if (lastResponse[t.id] === resp) {
      e.detail.shouldSwap = false; // fragment targets (Activity): compare the response itself
      return;
    }
    if (t.id) lastResponse[t.id] = resp;
  });

  // Keep each log box's open state + scroll position (keyed by data-log) across a swap so live
  // logs don't spring shut or yank you to the top. Auto-follow only if you were already at the
  // bottom.
  var logState = {};
  document.addEventListener("htmx:beforeSwap", function (e) {
    if (!isPolled(e.target)) return;
    e.target.querySelectorAll("details[data-log]").forEach(function (d) {
      var pre = d.querySelector("pre");
      var atBottom = pre && (pre.scrollHeight - pre.scrollTop - pre.clientHeight < 8);
      logState[d.getAttribute("data-log")] = { open: d.open, scroll: pre ? pre.scrollTop : 0, atBottom: atBottom };
    });
  });
  document.addEventListener("htmx:afterSwap", function (e) {
    if (!isPolled(e.target)) return;
    e.target.querySelectorAll("details[data-log]").forEach(function (d) {
      var st = logState[d.getAttribute("data-log")];
      if (!st) return;
      d.open = st.open;
      var pre = d.querySelector("pre");
      if (pre) pre.scrollTop = st.atBottom ? pre.scrollHeight : st.scroll;
    });
  });

  // Show/hide the live Activity tray (browser-downloads style).
  window.belayToggleTray = function () {
    const tray = document.getElementById("activity-tray");
    const toggle = document.getElementById("tray-toggle");
    if (!tray) return;
    tray.classList.toggle("hidden");
    if (toggle) toggle.style.display = tray.classList.contains("hidden") ? "" : "none";
  };
})();
