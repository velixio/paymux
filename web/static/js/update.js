// Follows an install on the Update page: polls its status, and reloads when
// it has ended or when the hub has come back after restarting.
(() => {
  const box = document.getElementById("update-status");
  if (!box || !["running", "restarting"].includes(box.dataset.state)) return;
  const label = box.querySelector("[data-label]");
  let down = false;
  async function tick() {
    try {
      const r = await fetch("/dashboard/update/status", { credentials: "same-origin", cache: "no-store" });
      if (!r.ok) throw new Error("status " + r.status);
      const s = await r.json();
      if (down || s.state === "idle" || s.state === "failed") {
        location.reload();
        return;
      }
      if (label && s.state === "restarting") label.textContent = s.to + " is installed. Paymux is restarting onto it; this page reloads when it is back.";
    } catch (e) {
      down = true;
      if (label) label.textContent = "Paymux is restarting...";
    }
    setTimeout(tick, 1500);
  }
  setTimeout(tick, 1000);
})();
