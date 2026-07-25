// Belay UI: a styled confirm modal (with optional changelog link) + client-side service search.
(function () {
  function modal(question, onOK, changelog) {
    const ov = document.createElement("div");
    ov.className = "modal-overlay";
    ov.innerHTML =
      '<div class="modal" role="dialog" aria-modal="true">' +
      '<p class="modal-q"></p>' +
      '<div class="modal-actions">' +
      '<button class="btn ghost" data-cancel>Cancel</button>' +
      '<button class="btn" data-ok>Update</button>' +
      "</div></div>";
    ov.querySelector(".modal-q").textContent = question;
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

  // Per-service Update buttons set hx-confirm — route that through our modal, with the changelog link.
  document.addEventListener("htmx:confirm", function (e) {
    if (!e.detail.question) return;
    e.preventDefault();
    const cl = e.detail.elt && e.detail.elt.getAttribute("data-changelog");
    modal(e.detail.question, () => e.detail.issueRequest(true), cl);
  });

  // "Update all" is a plain form; confirm via the same modal.
  window.belayConfirm = function (question, form) {
    modal(question, () => form.submit());
    return false;
  };

  // Filter service rows on the Updates tab.
  window.belayFilter = function (q) {
    q = q.toLowerCase();
    document.querySelectorAll('tr[id^="row-"]').forEach(function (tr) {
      tr.style.display = tr.textContent.toLowerCase().indexOf(q) >= 0 ? "" : "none";
    });
  };
})();
