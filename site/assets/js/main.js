// Agalma site: theme toggle, docs menu, copy buttons, tabs, table of contents highlight and search.

const root = document.documentElement;

// Theme: follows the system until the visitor picks one; the choice is remembered.
document.querySelector("[data-theme-toggle]")?.addEventListener("click", () => {
  const dark = root.dataset.theme
    ? root.dataset.theme === "dark"
    : matchMedia("(prefers-color-scheme: dark)").matches;
  root.dataset.theme = dark ? "light" : "dark";
  try { localStorage.setItem("theme", root.dataset.theme); } catch {}
});

// Docs menu on small screens.
const navToggle = document.querySelector(".nav-toggle");
navToggle?.addEventListener("click", () => {
  const open = document.body.classList.toggle("nav-open");
  navToggle.setAttribute("aria-expanded", String(open));
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && document.body.classList.contains("nav-open")) navToggle?.click();
});

// Copy buttons on code blocks.
const copyIcon = '<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h8"/></svg>';
const checkIcon = '<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m5 12.5 4.5 4.5L19 7.5"/></svg>';
for (const block of document.querySelectorAll(".code")) {
  const code = block.querySelector("pre code") ?? block.querySelector("pre");
  if (!code || !navigator.clipboard) continue;
  const button = document.createElement("button");
  button.type = "button";
  button.className = "copy-button";
  button.setAttribute("aria-label", "Copy code");
  button.innerHTML = copyIcon;
  button.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(code.innerText.replace(/\n$/, ""));
      button.classList.add("copied");
      button.innerHTML = checkIcon;
      setTimeout(() => { button.classList.remove("copied"); button.innerHTML = copyIcon; }, 1600);
    } catch {}
  });
  block.append(button);
}

// Tabs (WAI-ARIA tabs pattern with automatic activation).
for (const tabs of document.querySelectorAll("[data-tabs]")) {
  const list = [...tabs.querySelectorAll('[role="tab"]')];
  const select = (tab) => {
    for (const t of list) {
      const on = t === tab;
      t.setAttribute("aria-selected", String(on));
      t.tabIndex = on ? 0 : -1;
      document.getElementById(t.getAttribute("aria-controls")).hidden = !on;
    }
  };
  list.forEach((tab, i) => {
    tab.addEventListener("click", () => select(tab));
    tab.addEventListener("keydown", (e) => {
      const step = { ArrowRight: 1, ArrowLeft: -1 }[e.key];
      if (!step) return;
      const next = list[(i + step + list.length) % list.length];
      select(next);
      next.focus();
    });
  });
}

// Highlight the section being read in the "On this page" list.
const tocLinks = [...document.querySelectorAll(".docs-toc a")];
if (tocLinks.length && "IntersectionObserver" in window) {
  const byId = new Map(tocLinks.map((a) => [decodeURIComponent(a.hash.slice(1)), a]));
  const headings = [...byId.keys()].map((id) => document.getElementById(id)).filter(Boolean);
  const visible = new Set();
  const update = () => {
    const current = headings.find((h) => visible.has(h)) ?? null;
    if (!current) return;
    for (const a of tocLinks) a.classList.toggle("active", a === byId.get(current.id));
  };
  const io = new IntersectionObserver((entries) => {
    for (const e of entries) e.isIntersecting ? visible.add(e.target) : visible.delete(e.target);
    update();
  }, { rootMargin: "-60px 0px -65% 0px" });
  headings.forEach((h) => io.observe(h));
}

// Search over the index built by layouts/home.json, loaded on first use.
const dialog = document.querySelector(".search-dialog");
const input = dialog?.querySelector("input");
const results = dialog?.querySelector(".search-results");
const empty = dialog?.querySelector(".search-empty");
const indexURL = document.currentScript?.dataset.index ?? document.querySelector("script[data-index]")?.dataset.index;
let index = null;
let selected = -1;

async function loadIndex() {
  if (index) return index;
  const res = await fetch(indexURL);
  index = (await res.json()).map((p) => ({ ...p, lowTitle: p.title.toLowerCase(), lowText: p.text.toLowerCase() }));
  return index;
}

function openSearch() {
  if (!dialog || dialog.open) return;
  dialog.showModal();
  input.select();
  loadIndex().then(() => input.value && runSearch());
}

document.querySelectorAll("[data-search-open]").forEach((b) => b.addEventListener("click", openSearch));
dialog?.querySelector("[data-search-close]")?.addEventListener("click", () => dialog.close());
dialog?.addEventListener("click", (e) => { if (e.target === dialog) dialog.close(); });
document.addEventListener("keydown", (e) => {
  const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName) || e.target.isContentEditable;
  if ((e.key === "/" && !typing) || (e.key === "k" && (e.metaKey || e.ctrlKey))) {
    e.preventDefault();
    openSearch();
  }
});

const escapeHTML = (s) => s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
const escapeRE = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

function mark(text, terms) {
  const re = new RegExp(`(${terms.map(escapeRE).join("|")})`, "gi");
  return escapeHTML(text).replace(re, "<mark>$1</mark>");
}

function snippet(page, terms) {
  const at = page.lowText.indexOf(terms[0]);
  if (at < 0) return "";
  const start = Math.max(0, page.text.lastIndexOf(" ", Math.max(0, at - 60)) + 1);
  const text = page.text.slice(start, start + 180).replace(/\s+/g, " ");
  return (start > 0 ? "… " : "") + text + "…";
}

function runSearch() {
  if (!index) return;
  const terms = input.value.toLowerCase().split(/\s+/).filter((t) => t.length > 1);
  results.innerHTML = "";
  selected = -1;
  if (!terms.length) { empty.hidden = true; return; }
  const all = (s) => terms.every((t) => s.includes(t));
  const found = [];
  for (const page of index) {
    if (all(page.lowTitle)) found.push({ score: 100, url: page.url, title: page.title, path: page.group, text: snippet(page, terms) });
    for (const h of page.headings) {
      const low = h.text.toLowerCase();
      if (all(low)) found.push({ score: 50 + (low.startsWith(terms[0]) ? 5 : 0), url: `${page.url}#${h.id}`, title: h.text, path: `${page.group} › ${page.title}`, text: "" });
    }
    if (!all(page.lowTitle) && all(page.lowText)) {
      const hits = terms.reduce((n, t) => n + page.lowText.split(t).length - 1, 0);
      found.push({ score: Math.min(40, 10 + hits), url: page.url, title: page.title, path: page.group, text: snippet(page, terms) });
    }
  }
  found.sort((a, b) => b.score - a.score);
  const top = found.slice(0, 12);
  empty.hidden = top.length > 0;
  results.innerHTML = top.map((r, i) => `
    <li role="option" id="r-${i}" aria-selected="false">
      <a href="${r.url}">
        <span class="r-path">${escapeHTML(r.path)}</span>
        <span class="r-title">${mark(r.title, terms)}</span>
        ${r.text ? `<span class="r-snippet">${mark(r.text, terms)}</span>` : ""}
      </a>
    </li>`).join("");
  if (top.length) move(0);
}

function move(to) {
  const items = [...results.children];
  if (!items.length) return;
  selected = (to + items.length) % items.length;
  items.forEach((li, i) => li.setAttribute("aria-selected", String(i === selected)));
  items[selected].scrollIntoView({ block: "nearest" });
  input.setAttribute("aria-activedescendant", items[selected].id);
}

input?.addEventListener("input", () => loadIndex().then(runSearch));
input?.addEventListener("keydown", (e) => {
  if (e.key === "ArrowDown") { e.preventDefault(); move(selected + 1); }
  else if (e.key === "ArrowUp") { e.preventDefault(); move(selected - 1); }
  else if (e.key === "Enter") {
    e.preventDefault();
    const link = results.children[selected]?.querySelector("a");
    if (link) { dialog.close(); location.href = link.href; }
  }
});
results?.addEventListener("click", (e) => { if (e.target.closest("a")) dialog.close(); });
