// Belay UI: a styled confirm modal (replacing the browser's) + client-side service search.
(function () {
  function modal(question, onOK) {
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
    document.body.appendChild(ov);
    const close = () => ov.remove();
    ov.addEventListener("click", (e) => { if (e.target === ov) close(); });
    function esc(e) { if (e.key === "Escape") { close(); document.removeEventListener("keydown", esc); } }
    document.addEventListener("keydown", esc);
    ov.querySelector("[data-cancel]").onclick = close;
    ov.querySelector("[data-ok]").onclick = () => { close(); onOK(); };
    ov.querySelector("[data-ok]").focus();
  }

  // Per-service Update buttons set hx-confirm — route that through our modal instead of window.confirm.
  document.addEventListener("htmx:confirm", function (e) {
    if (!e.detail.question) return;
    e.preventDefault();
    modal(e.detail.question, () => e.detail.issueRequest(true));
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
