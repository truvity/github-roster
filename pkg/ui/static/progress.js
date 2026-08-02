// Progress feedback while a plan computes or applies: disable resubmit,
// show elapsed seconds. The server round-trip IS the operation.
document.querySelectorAll("form").forEach(function (form) {
  form.addEventListener("submit", function () {
    var button = form.querySelector("button[type=submit]");
    var progress = form.querySelector("[data-progress]");
    if (button) { button.disabled = true; }
    if (!progress) { return; }
    progress.hidden = false;
    var seconds = 0;
    var counter = progress.querySelector("[data-elapsed]");
    setInterval(function () { seconds++; if (counter) { counter.textContent = seconds; } }, 1000);
  });
});
