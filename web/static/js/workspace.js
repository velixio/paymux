"use strict";
(function () {
  const button = document.querySelector(".workspace-menu");
  const nav = document.getElementById("workspace-nav");
  if (!button || !nav) return;
  const media = window.matchMedia("(max-width: 760px)");
  function sync() {
    nav.hidden = media.matches;
    button.setAttribute("aria-expanded", String(!nav.hidden));
  }
  button.addEventListener("click", function () {
    nav.hidden = !nav.hidden;
    button.setAttribute("aria-expanded", String(!nav.hidden));
  });
  media.addEventListener("change", sync);
  sync();
})();

(function () {
  const table = document.querySelector("table.settings");
  if (table) {
    const groups = [
      ...new Set(
        [...table.querySelectorAll("[data-setting-group]")].map(
          (row) => row.dataset.settingGroup,
        ),
      ),
    ];
    const nav = document.createElement("nav");
    nav.className = "setting-categories";
    nav.setAttribute("aria-label", "Policy categories");
    function show(group) {
      table.querySelectorAll("[data-setting-group]").forEach((row) => {
        row.hidden = row.dataset.settingGroup !== group;
      });
      nav
        .querySelectorAll("button")
        .forEach((b) =>
          b.setAttribute("aria-pressed", String(b.textContent === group)),
        );
    }
    groups.forEach((group) => {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = group;
      b.onclick = () => show(group);
      nav.append(b);
    });
    table.parentElement.before(nav);
    if (groups.length) show(groups[0]);
    function revealHash() {
      const row = document.getElementById(location.hash.slice(1));
      if (row && row.dataset.settingGroup) show(row.dataset.settingGroup);
    }
    window.addEventListener("hashchange", revealHash);
    revealHash();
  }
  document
    .querySelectorAll('a[href="#routing"], a[href="#route-test"]')
    .forEach((a) =>
      a.addEventListener("click", () => {
        const details = document.querySelector(".routing-disclosure");
        if (details) details.open = true;
      }),
    );
  if (document.querySelector(".route-result")) {
    const details = document.querySelector(".routing-disclosure");
    if (details) details.open = true;
  }
  const dirty = new Set();
  let navigating = false;
  document.querySelectorAll(".setting-form").forEach((form) => {
    const initial = new URLSearchParams(new FormData(form)).toString();
    form.addEventListener("input", () => {
      const changed =
        initial !== new URLSearchParams(new FormData(form)).toString();
      form.classList.toggle("dirty", changed);
      if (changed) dirty.add(form);
      else dirty.delete(form);
    });
  });
  window.addEventListener("beforeunload", (event) => {
    if (dirty.size && !navigating) {
      event.preventDefault();
      event.returnValue = "";
    }
  });
  const dialog = document.getElementById("change-review");
  if (!dialog) return;
  let pending, submitter;
  const approved = new WeakSet();
  document.addEventListener("submit", (event) => {
    const form = event.target;
    if (approved.has(form)) {
      navigating = true;
      return;
    }
    const path = new URL(form.action).pathname;
    const risky =
      form.classList.contains("setting-form") ||
      /\/(refund|revoke|rotate|accept|submit|replay)$/.test(path) ||
      path.endsWith("/settings/routing") ||
      /\/settings\/[^/]+\/reset$/.test(path);
    if (!risky) {
      navigating = true;
      return;
    }
    event.preventDefault();
    pending = form;
    submitter = event.submitter;
    const row = form.closest("tr");
    const label =
      row?.querySelector(".setting-name")?.textContent ||
      submitter?.textContent.trim() ||
      "Update configuration";
    const scope = path.includes("/apps/")
      ? "Selected app"
      : path.includes("/settings")
        ? "Organization · all apps without an override"
        : "Selected record";
    const value = new FormData(form).getAll("value").at(-1);
    document.getElementById("change-description").textContent =
      label +
      "\nScope: " +
      scope +
      (value ? "\nNew value: " + value : "") +
      (path.endsWith("/refund")
        ? "\nRefund amount: " +
          (new FormData(form).get("amount") || "All remaining refundable funds")
        : "") +
      (path.endsWith("/reset")
        ? "\nRemove this app override and inherit the organization value."
        : "");
    dialog.showModal();
    document.getElementById("change-cancel").focus();
  });
  function cancel() {
    dialog.close();
    submitter?.focus();
    pending = null;
  }
  document.getElementById("change-cancel").onclick = cancel;
  dialog.addEventListener("cancel", (event) => {
    event.preventDefault();
    cancel();
  });
  document.getElementById("change-confirm").onclick = () => {
    if (!pending) return;
    const form = pending;
    approved.add(form);
    navigating = true;
    dialog.close();
    form.requestSubmit(submitter);
  };
})();
