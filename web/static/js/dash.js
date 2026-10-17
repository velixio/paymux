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
