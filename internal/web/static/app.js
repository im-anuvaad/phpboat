// phpboat dashboard: live log streams, status refresh and confirmations.
// Everything also works without JavaScript except the live parts.
"use strict";

// <pre data-stream="/url"> shows a Server-Sent Events stream.
function stream(pre) {
  const status = document.querySelector("[data-stream-status]");
  const es = new EventSource(pre.dataset.stream);
  const maxLines = 3000;
  let lines = 0;
  const atBottom = () => pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
  es.onopen = () => { if (status) status.textContent = "live"; };
  es.onmessage = (e) => {
    const stick = atBottom();
    pre.appendChild(document.createTextNode(e.data + "\n"));
    lines += e.data.split("\n").length;
    while (lines > maxLines && pre.firstChild) {
      pre.removeChild(pre.firstChild);
      lines = pre.textContent.split("\n").length;
    }
    if (stick) pre.scrollTop = pre.scrollHeight;
  };
  es.addEventListener("done", (e) => {
    es.close();
    if (status) status.textContent = "finished: " + e.data;
    if (pre.hasAttribute("data-reload-on-done")) setTimeout(() => location.reload(), 800);
  });
  es.onerror = () => { if (status) status.textContent = "reconnecting…"; };
}

// Elements with data-refresh="N" and an id are re-fetched every N seconds,
// unless the user is typing in them.
function refresh(el) {
  const every = Math.max(2, parseInt(el.dataset.refresh, 10) || 5) * 1000;
  setInterval(async () => {
    if (document.hidden || el.contains(document.activeElement)) return;
    try {
      const r = await fetch(location.href, { credentials: "same-origin", headers: { "Accept": "text/html" } });
      if (!r.ok || r.redirected) return;
      const doc = new DOMParser().parseFromString(await r.text(), "text/html");
      const fresh = doc.getElementById(el.id);
      if (fresh) { el.innerHTML = fresh.innerHTML; wire(el); }
    } catch (_) { /* offline: try again later */ }
  }, every);
}

// <select data-branch-source="#repo" data-branch-note="id"> lists the branches
// of the repository typed into the input, from /github/branches. On the Add
// app form the empty option means the repository's default branch; the
// settings form (data-no-default) always has a branch.
function branchPicker(sel) {
  const input = document.querySelector(sel.dataset.branchSource);
  if (!input) return;
  const note = document.getElementById(sel.dataset.branchNote || "");
  const say = (text) => { if (note) note.textContent = text; };
  const load = async () => {
    const repo = input.value.trim();
    if (!repo) return;
    try {
      const r = await fetch("/github/branches?repo=" + encodeURIComponent(repo), {
        credentials: "same-origin", headers: { "Accept": "application/json" },
      });
      const j = await r.json();
      if (!r.ok) throw new Error(j.error || "the lookup failed");
      const keep = sel.value;
      sel.innerHTML = "";
      if (!sel.hasAttribute("data-no-default")) {
        sel.add(new Option("default branch (" + j.default + ")", ""));
      }
      for (const b of j.branches) sel.add(new Option(b, b));
      if (keep && !j.branches.includes(keep)) sel.add(new Option(keep, keep));
      sel.value = keep;
      say(j.branches.length + " branches. Default: " + j.default + ".");
    } catch (e) {
      say("Could not list the branches: " + e.message);
    }
  };
  input.addEventListener("change", load);
  if (input.value.trim()) load();
}

function wire(root) {
  root.querySelectorAll("form[data-confirm]").forEach((f) => {
    f.addEventListener("submit", (e) => { if (!confirm(f.dataset.confirm)) e.preventDefault(); });
  });
  root.querySelectorAll("[data-confirm-click]").forEach((b) => {
    b.addEventListener("click", (e) => { if (!confirm(b.dataset.confirmClick)) e.preventDefault(); });
  });
}

document.addEventListener("DOMContentLoaded", () => {
  document.querySelectorAll("pre[data-stream]").forEach(stream);
  document.querySelectorAll("[data-refresh][id]").forEach(refresh);
  document.querySelectorAll("select[data-branch-source]").forEach(branchPicker);
  wire(document);
});
