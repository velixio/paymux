// Copies the value of the input named by a button's data-copy attribute.
document.addEventListener("click", function (ev) {
  var btn = ev.target.closest("[data-copy]");
  if (!btn) return;
  var input = document.getElementById(btn.getAttribute("data-copy"));
  if (!input) return;
  input.select();
  if (navigator.clipboard) {
    navigator.clipboard.writeText(input.value).then(function () { btn.textContent = "Copied"; });
  } else {
    document.execCommand("copy");
    btn.textContent = "Copied";
  }
});
