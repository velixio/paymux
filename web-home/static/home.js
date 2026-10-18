"use strict";
document.querySelectorAll("[data-copy]").forEach((button) => {
  button.addEventListener("click", async () => {
    const text = document.getElementById(button.dataset.copy)?.textContent;
    if (!text) return;
    try {
      await navigator.clipboard.writeText(text);
      button.textContent = "Copied";
      setTimeout(() => (button.textContent = "Copy"), 1800);
    } catch {
      button.textContent = "Select code to copy";
    }
  });
  button.setAttribute("aria-live", "polite");
});
