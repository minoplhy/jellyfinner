// Jellyfinner admin UI helpers. The page's CSP allows scripts only from this
// origin and htmx runs with allowEval off, so everything that needs a little
// logic lives here, wired by data- attributes instead of inline handlers.
(function () {
  "use strict";
  var expired = false;

  // Polls skip a beat while the tab is hidden, after the session expires, and
  // while someone is working inside the polled area, so a refresh never
  // throws away a ticked box or a focused field.
  document.addEventListener("htmx:beforeRequest", function (e) {
    var elt = e.detail.elt;
    if (!elt.hasAttribute || !elt.hasAttribute("data-poll")) return;
    if (expired || document.hidden) { e.preventDefault(); return; }
    if (elt.contains(document.activeElement) && document.activeElement !== elt) { e.preventDefault(); return; }
    if (elt.querySelector("input[type=checkbox]:checked")) e.preventDefault();
  });

  document.addEventListener("htmx:responseError", function (e) {
    var status = e.detail.xhr ? e.detail.xhr.status : 0;
    if (status === 401) {
      expired = true;
      var banner = document.getElementById("session-expired");
      if (banner) banner.hidden = false;
      return;
    }
    toast("error", "Request failed (" + status + "). Reload the page and try again.");
  });

  document.addEventListener("htmx:sendError", function () {
    toast("error", "Could not reach the orchestrator. Check the connection.");
  });

  function toast(kind, text) {
    var box = document.getElementById("toasts");
    if (!box) return;
    var el = document.createElement("div");
    el.className = "toast toast-" + kind;
    el.setAttribute("role", "status");
    el.textContent = text;
    box.appendChild(el);
  }

  // Toasts fade out by CSS. Remove them once the animation ends.
  document.addEventListener("animationend", function (e) {
    if (e.target.classList && e.target.classList.contains("toast")) e.target.remove();
  });

  // Select all: a checkbox with data-select-all ticks every path box in its
  // form, and [data-selected] shows how many are ticked.
  function refreshSelection(form) {
    var boxes = form.querySelectorAll("input[type=checkbox][name=path]");
    var ticked = 0;
    boxes.forEach(function (b) { if (b.checked) ticked++; });
    var all = form.querySelector("[data-select-all]");
    if (all) {
      all.checked = boxes.length > 0 && ticked === boxes.length;
      all.indeterminate = ticked > 0 && ticked < boxes.length;
    }
    form.querySelectorAll("[data-selected]").forEach(function (el) {
      el.textContent = ticked ? ticked + " selected" : "";
    });
  }

  document.addEventListener("change", function (e) {
    var t = e.target;
    var form = t.form || (t.closest && t.closest("form"));
    if (!form) return;
    if (t.hasAttribute("data-select-all")) {
      form.querySelectorAll("input[type=checkbox][name=path]").forEach(function (b) { b.checked = t.checked; });
    }
    if (t.name === "path" || t.hasAttribute("data-select-all")) refreshSelection(form);
  });

  // New rows arrive without ticks, so the counter starts over. An outerHTML
  // swap detaches the old target, so look the forms up again.
  document.addEventListener("htmx:afterSettle", function () {
    document.querySelectorAll("[data-select-all]").forEach(function (all) {
      if (all.form) refreshSelection(all.form);
    });
  });
})();
