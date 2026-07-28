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

  // Show/hide the live Activity tray (browser-downloads style).
  window.belayToggleTray = function () {
    const tray = document.getElementById("activity-tray");
    const toggle = document.getElementById("tray-toggle");
    if (!tray) return;
    tray.classList.toggle("hidden");
    if (toggle) toggle.style.display = tray.classList.contains("hidden") ? "" : "none";
  };
})();
