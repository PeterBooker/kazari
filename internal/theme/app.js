// Copy buttons, local times and passkeys. Every page works without this
// file, except that a passkey needs it.
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
    var label = button.textContent, timer;
    function show(word, said, ms) {
      clearTimeout(timer);
      button.textContent = word;
      live.textContent = said;
      timer = setTimeout(function () {
        button.textContent = label;
        live.textContent = "";
      }, ms);
    }
    button.hidden = false;
    button.addEventListener("click", function () {
      copy(target).then(function () {
        show("Copied", "Copied", 2000);
      }, function () {
        show("Could not copy", "Could not copy. Select the text and copy it.", 5000);
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

  // A passkey form carries its ceremony's options in data-passkey-options
  // and stays hidden until the browser can answer them. The answer goes
  // back in the form's passkey field, as JSON in the form WebAuthn names.
  if (!window.PublicKeyCredential || !navigator.credentials) return;

  function decode(s) {
    var b = atob(s.replace(/-/g, "+").replace(/_/g, "/")), out = new Uint8Array(b.length);
    for (var i = 0; i < b.length; i++) out[i] = b.charCodeAt(i);
    return out.buffer;
  }

  function encode(buf) {
    var b = "", bytes = new Uint8Array(buf);
    for (var i = 0; i < bytes.length; i++) b += String.fromCharCode(bytes[i]);
    return btoa(b).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function ids(list) {
    (list || []).forEach(function (c) { c.id = decode(c.id); });
  }

  function options(raw, create) {
    var o = JSON.parse(raw);
    o.challenge = decode(o.challenge);
    if (create) {
      o.user.id = decode(o.user.id);
      ids(o.excludeCredentials);
    } else {
      ids(o.allowCredentials);
    }
    return o;
  }

  function answer(cred) {
    var r = cred.response, out = {
      id: cred.id,
      rawId: encode(cred.rawId),
      type: cred.type,
      clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
      response: { clientDataJSON: encode(r.clientDataJSON) }
    };
    if (cred.authenticatorAttachment) out.authenticatorAttachment = cred.authenticatorAttachment;
    if (r.attestationObject) {
      out.response.attestationObject = encode(r.attestationObject);
      if (r.getTransports) out.response.transports = r.getTransports();
    } else {
      out.response.authenticatorData = encode(r.authenticatorData);
      out.response.signature = encode(r.signature);
      if (r.userHandle) out.response.userHandle = encode(r.userHandle);
    }
    return JSON.stringify(out);
  }

  document.querySelectorAll("[data-passkey-js]").forEach(function (el) { el.hidden = false; });

  // A form that asks for the current password takes one of the person's
  // passkeys instead when the field is left empty.
  function confirmFirst(form) {
    var raw = form.getAttribute("data-passkey-confirm");
    var current = form.querySelector('input[name="current"]');
    var field = form.querySelector('input[name="confirm_passkey"]');
    var error = form.querySelector("[data-passkey-confirm-error]");
    if (error) error.hidden = true;
    if (!raw || !field || (current && current.value)) return Promise.resolve();
    return navigator.credentials.get({ publicKey: options(raw, false) }).then(function (cred) {
      field.value = answer(cred);
    }, function (err) {
      field.value = "";
      if (error) error.hidden = false;
      throw err;
    });
  }

  document.querySelectorAll("form[data-passkey-confirm]:not([data-passkey])").forEach(function (form) {
    form.addEventListener("submit", function (e) {
      var current = form.querySelector('input[name="current"]');
      if (current && current.value) return;
      e.preventDefault();
      confirmFirst(form).then(function () { form.submit(); }, function () {});
    });
  });

  document.querySelectorAll("form[data-passkey]").forEach(function (form) {
    var create = form.getAttribute("data-passkey") === "create";
    var raw = form.getAttribute("data-passkey-options");
    var field = form.querySelector('input[name="passkey"]');
    var error = form.querySelector("[data-passkey-error]");
    var waiting = null;
    if (!raw || !field) return;
    form.hidden = false;
    document.querySelectorAll("[data-passkey-nojs]").forEach(function (el) { el.hidden = true; });

    function send(cred) {
      field.value = answer(cred);
      form.submit();
    }

    // The browser checks the form's required fields before this runs.
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      if (waiting) waiting.abort();
      if (error) error.hidden = true;
      var confirmed = create ? confirmFirst(form) : Promise.resolve();
      confirmed.then(function () {
        var ask = create
          ? navigator.credentials.create({ publicKey: options(raw, true) })
          : navigator.credentials.get({ publicKey: options(raw, false) });
        ask.then(send, function () {
          if (error) error.hidden = false;
        });
      }, function () {});
    });

    // On the sign-in page, offer the passkeys in the name field's autofill.
    var autofill = document.querySelector('input[autocomplete~="webauthn"]');
    if (create || !autofill || !PublicKeyCredential.isConditionalMediationAvailable || !window.AbortController) return;
    PublicKeyCredential.isConditionalMediationAvailable().then(function (ok) {
      if (!ok) return;
      waiting = new AbortController();
      navigator.credentials.get({ publicKey: options(raw, false), mediation: "conditional", signal: waiting.signal })
        .then(send, function () {});
    });
  });
})();
