// Live run transcript: replay-then-tail over SSE. Reconnects (including
// gateway timeouts mid-stream) replay from the start, so we clear on open.
(function () {
  var transcript = document.getElementById("transcript");
  var verdict = document.getElementById("verdict");
  var source = new EventSource("/sync/run/stream?org=" +
    encodeURIComponent(transcript.dataset.org) + "&id=" + encodeURIComponent(transcript.dataset.run));

  source.addEventListener("line", function (e) {
    transcript.textContent += e.data + "\n";
    transcript.scrollTop = transcript.scrollHeight;
  });

  source.addEventListener("done", function (e) {
    source.close();
    verdict.hidden = false;
    verdict.innerHTML = e.data === "succeeded"
      ? '<span class="badge ok">succeeded</span>'
      : '<span class="badge danger">' + e.data + '</span> — see the transcript and the audit record';
  });

  source.onopen = function () { transcript.textContent = ""; };
})();
