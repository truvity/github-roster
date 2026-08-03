// Picking a directory person prefills what the platform already knows —
// the IdP addresses and the conventional namespace — but only into EMPTY
// fields; typed values are never overwritten.
//
// Served from /static/ because the CSP allows no inline scripts. Listens
// to input AND change: Safari does not reliably fire change on a datalist
// pick, and on input the value only matches an option once a pick (or a
// complete manual type-out) happened.
(function () {
  "use strict";

  var name = document.getElementById("name");
  if (!name) return;

  function prefill() {
    var options = document.querySelectorAll("#directory-people option");
    var opt = null;
    for (var i = 0; i < options.length; i++) {
      if (options[i].value === name.value) {
        opt = options[i];
        break;
      }
    }
    if (!opt) return;

    var emails = document.getElementById("emails");
    if (emails && !emails.value) emails.value = opt.dataset.emails || "";

    var k8s = document.getElementById("k8s");
    if (k8s && !k8s.value) k8s.value = opt.dataset.k8s || "";
  }

  name.addEventListener("input", prefill);
  name.addEventListener("change", prefill);
})();
