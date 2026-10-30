// Copy-to-clipboard buttons: <button data-copy="text">.
document.addEventListener("click", function (e) {
  var b = e.target.closest("[data-copy]");
  if (!b || !navigator.clipboard) return;
  navigator.clipboard.writeText(b.getAttribute("data-copy")).then(function () {
    var old = b.textContent;
    b.textContent = "Copied";
    setTimeout(function () { b.textContent = old; }, 1500);
  });
});

// Provider webhook secret generators. The value stays in the form until the
// operator saves it; secure random generation is required.
document.addEventListener("click", function (e) {
  var b = e.target.closest("[data-generate-secret]");
  if (!b) return;
  var name = b.getAttribute("data-generate-secret");
  var input = document.querySelector('[name="' + name + '"]');
  var help = document.querySelector('[data-secret-help="' + name + '"]');
  if (!input || !window.crypto || !window.crypto.getRandomValues) {
    if (help) help.textContent = "Secure generation is unavailable in this browser.";
    return;
  }
  var bytes = new Uint8Array(20);
  window.crypto.getRandomValues(bytes);
  input.value = Array.from(bytes, function (n) {
    return n.toString(16).padStart(2, "0");
  }).join("");
  input.type = "text";
  input.focus();
  input.select();
  b.textContent = "Generate another";
  if (help) help.textContent = "Generated and selected. Copy it to the provider dashboard, then save these settings.";
});
