// Copy buttons and local times. Every page works without this file.
(function () {
  "use strict";

  var live = document.createElement("div");
  live.className = "sr-only";
  live.setAttribute("aria-live", "polite");
  document.body.appendChild(live);

  function text(el) {
    return "value" in el ? el.value : el.textContent;
  }

  function copy(el) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text(el));
    }
    // Plain http on a name that is not localhost has no clipboard API.
    return new Promise(function (resolve, reject) {
      var range = document.createRange();
      range.selectNodeContents(el);
      var sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      if ("select" in el) el.select();
      document.execCommand("copy") ? resolve() : reject();
    });
  }

  document.querySelectorAll("[data-copy-target]").forEach(function (button) {
    var target = document.getElementById(button.getAttribute("data-copy-target"));
    if (!target) return;
    var label = button.textContent;
    button.hidden = false;
    button.addEventListener("click", function () {
      copy(target).then(function () {
        button.textContent = "Copied";
        live.textContent = "Copied";
        setTimeout(function () {
          button.textContent = label;
          live.textContent = "";
        }, 2000);
      }, function () {
        live.textContent = "Could not copy. Select the text and copy it.";
      });
    });
  });

  document.querySelectorAll("input[readonly]").forEach(function (input) {
    input.addEventListener("focus", function () { input.select(); });
  });

  var minute = 60e3, hour = 60 * minute, day = 24 * hour;
  var relative = "RelativeTimeFormat" in Intl ? new Intl.RelativeTimeFormat("en-GB", { numeric: "auto" }) : null;

  function midnight(d) {
    return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  }

  function clock(d) {
    return d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
  }

  function describe(d, now) {
    var diff = d.getTime() - now.getTime(), abs = Math.abs(diff);
    if (abs < minute) return "just now";
    if (abs < hour) return relative.format(Math.round(diff / minute), "minute");
    var days = Math.round((midnight(d) - midnight(now)) / day);
    if (days === 0 && abs < 6 * hour) return relative.format(Math.round(diff / hour), "hour");
    if (Math.abs(days) <= 1) return relative.format(days, "day") + ", " + clock(d);
    var opts = { day: "numeric", month: "long" };
    if (d.getFullYear() !== now.getFullYear()) opts.year = "numeric";
    return d.toLocaleDateString("en-GB", opts);
  }

  function times() {
    var now = new Date();
    document.querySelectorAll("time[datetime]").forEach(function (el) {
      var d = new Date(el.getAttribute("datetime"));
      if (isNaN(d)) return;
      if (!el.title) el.title = el.textContent.trim();
      el.textContent = describe(d, now);
    });
  }

  if (relative) {
    times();
    setInterval(times, minute);
  }
})();
