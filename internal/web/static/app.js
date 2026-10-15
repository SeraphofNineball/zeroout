"use strict";

const $ = (id) => document.getElementById(id);
const PATTERNS = [
  ["zero", "Zeros (0x00)"],
  ["one", "Ones (0xFF)"],
  ["random", "Random"],
];
const PRESETS = {
  zero: ["zero"],
  "random-zero": ["random", "zero"],
  three: ["one", "random", "zero"],
};
const MAX_PASSES = 35;

const state = {
  volumes: [],
  status: { current: null, resumable: null, history: [] },
  selected: null,
  passes: ["zero"],
  online: true,
};

// ---------- helpers ----------

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (method !== "GET") {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body ?? {});
  }
  const res = await fetch(path, opts);
  if (res.status === 204) return null;
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error((data && data.error) || `${res.status} ${res.statusText}`);
  return data;
}

function fmtBytes(n) {
  if (n == null || isNaN(n)) return "—";
  const units = ["B", "kB", "MB", "GB", "TB", "PB"];
  let i = 0;
  while (Math.abs(n) >= 1000 && i < units.length - 1) { n /= 1000; i++; }
  return `${n.toFixed(i === 0 ? 0 : n >= 100 ? 0 : 1)} ${units[i]}`;
}

function fmtDuration(sec) {
  if (!sec || sec < 0 || !isFinite(sec)) return "—";
  sec = Math.round(sec);
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${s}s`;
  return `${s}s`;
}

function fmtTime(iso) {
  return new Date(iso).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k === "style") e.style.cssText = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) e.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) if (c != null) e.append(c);
  return e;
}

function patternLabel(p) {
  const hit = PATTERNS.find(([v]) => v === p);
  return hit ? hit[1].split(" ")[0] : p;
}

function showError(msg) {
  const box = $("error");
  box.textContent = msg || "";
  box.hidden = !msg;
}

// ---------- volumes ----------

function renderVolumes() {
  const wrap = $("volumes");
  wrap.replaceChildren();
  $("no-volumes").hidden = state.volumes.length > 0;
  const busy = state.status.current;

  for (const v of state.volumes) {
    const usable = !v.error && v.isMount;
    const used = v.total - v.free;
    const pct = (x) => v.total ? (x / v.total * 100).toFixed(2) + "%" : "0";
    const isTarget = busy && busy.spec.volume === v.name;

    const card = el("button", {
      class: "vol" + (state.selected === v.name ? " selected" : ""),
      disabled: !usable || !!busy,
      onclick: () => selectVolume(v.name),
    },
      el("div", { class: "vol-head" },
        el("span", { class: "vol-name" }, v.name),
        isTarget ? el("span", { class: "tag" }, "Wiping") : el("span", { class: "vol-meta" }, [v.fsType, v.source].filter(Boolean).join(" · ")),
      ),
    );

    if (v.error) {
      card.append(el("div", { class: "vol-warn" }, v.error));
    } else {
      card.append(
        el("div", { class: "usage", title: `${fmtBytes(used)} used of ${fmtBytes(v.total)}` },
          el("div", { class: "u", style: `width:${pct(used)}` }),
          el("div", { class: "r", style: `width:${pct(v.reserved)}` }),
        ),
        el("div", { class: "legend" },
          el("span", {}, el("i", { style: "background:var(--used)" }), `${fmtBytes(used)} used`),
          el("span", {}, el("i", { style: "background:var(--surface-2);outline:1px solid var(--border)" }), `${fmtBytes(v.avail)} free`),
          v.reserved > 0 ? el("span", {}, el("i", { style: "background:var(--reserved)" }), `${fmtBytes(v.reserved)} reserved`) : null,
        ),
      );
      if (!v.isMount) {
        card.append(el("div", { class: "vol-warn" }, "Not a mount point — bind-mount a host volume here to wipe it."));
      }
    }
    wrap.append(card);
  }
}

function selectVolume(name) {
  state.selected = state.selected === name ? null : name;
  showError("");
  renderVolumes();
  renderSetup();
  if (state.selected) $("setup").scrollIntoView({ behavior: "smooth", block: "start" });
}

// ---------- setup form ----------

function selectedVolume() {
  return state.volumes.find((v) => v.name === state.selected);
}

function mode() {
  return document.querySelector('input[name="mode"]:checked').value;
}

function renderSetup() {
  const v = selectedVolume();
  const show = !!v && !state.status.current;
  $("setup").hidden = !show;
  if (!show) { state.setupFor = null; return; }
  $("setup-volume").textContent = v.name;
  $("reserved").textContent = fmtBytes(v.reserved);
  // Polls only refresh the numbers; rebuilding the selects would close an open dropdown.
  if (state.setupFor !== v.name) {
    state.setupFor = v.name;
    renderPasses();
  }
  renderEstimate();
}

function renderPasses() {
  const list = $("passes");
  list.replaceChildren();
  state.passes.forEach((p, i) => {
    const sel = el("select", {
      "aria-label": `Pass ${i + 1} pattern`,
      onchange: (e) => { state.passes[i] = e.target.value; renderEstimate(); },
    }, PATTERNS.map(([val, label]) => el("option", { value: val, selected: val === p }, label)));
    list.append(el("li", {},
      el("span", { class: "n" }, `Pass ${i + 1}`),
      sel,
      state.passes.length > 1
        ? el("button", { type: "button", class: "icon-btn", title: "Remove pass", "aria-label": `Remove pass ${i + 1}`, onclick: () => { state.passes.splice(i, 1); renderPasses(); renderEstimate(); } }, "✕")
        : null,
    ));
  });
  $("add-pass").disabled = state.passes.length >= MAX_PASSES;
}

function bytesPerPass(v) {
  if (mode() === "full") return v.free;
  const margin = Math.max(0, Number($("margin").value) || 0) * 1e9;
  return Math.max(0, v.avail - margin);
}

function renderEstimate() {
  const v = selectedVolume();
  if (!v) return;
  const per = bytesPerPass(v);
  const n = state.passes.length;
  const box = $("estimate");
  if (per <= 0) {
    box.textContent = "The margin is larger than the free space — nothing would be written.";
    $("start").disabled = true;
    return;
  }
  $("start").disabled = false;
  let text = `Writes about ${fmtBytes(per)} per pass`;
  if (n > 1) text += `, ${fmtBytes(per * n)} in total`;
  if ($("verify").checked) text += `, plus the same amount read back to verify`;
  box.textContent = text + ".";
}

async function startWipe() {
  const v = selectedVolume();
  if (!v) return;
  const spec = {
    volume: v.name,
    passes: [...state.passes],
    mode: mode(),
    marginGB: Math.max(0, Number($("margin").value) || 0),
    verify: $("verify").checked,
  };
  const list = $("confirm-list");
  list.replaceChildren(
    el("li", {}, `Volume: `, el("strong", {}, v.name), ` (${v.path})`),
    el("li", {}, `${spec.passes.length} pass${spec.passes.length > 1 ? "es" : ""}: ${spec.passes.map(patternLabel).join(", ")}`),
    el("li", {}, spec.mode === "full" ? "Fill completely, including reserved space" : `Leave ${spec.marginGB} GB free`),
    el("li", {}, spec.verify ? "Verify each pass" : "No verification"),
    el("li", {}, `About ${fmtBytes(bytesPerPass(v) * spec.passes.length)} to write`),
  );
  const dlg = $("confirm");
  dlg.returnValue = "";
  dlg.showModal();
  dlg.addEventListener("close", async () => {
    if (dlg.returnValue !== "ok") return;
    try {
      await api("POST", "/api/jobs", spec);
      state.selected = null;
      showError("");
      await refresh();
    } catch (e) {
      showError(`Could not start: ${e.message}`);
    }
  }, { once: true });
}

// ---------- running job ----------

const PHASE_LABEL = { writing: "Writing", verifying: "Verifying", cleaning: "Removing fill files" };

function renderRunning() {
  const j = state.status.current;
  $("running").hidden = !j;
  if (!j) return;
  const p = j.progress;
  $("run-volume").textContent = j.spec.volume;

  const pills = $("run-passes");
  pills.replaceChildren();
  j.spec.passes.forEach((pat, i) => {
    const cls = i < j.passesDone ? "done" : p && p.pass - 1 === i ? "current" : "";
    pills.append(el("li", { class: cls }, `${i + 1}. ${patternLabel(pat)}`));
  });

  const bar = $("run-bar");
  if (!p) {
    $("run-phase").textContent = "Starting…";
    $("run-percent").textContent = "";
    bar.classList.add("indeterminate");
  } else {
    const label = PHASE_LABEL[p.phase] || p.phase;
    $("run-phase").textContent = `Pass ${p.pass} of ${p.passes} · ${label}`;
    if (p.phase === "cleaning" || !p.total) {
      bar.classList.add("indeterminate");
      $("run-percent").textContent = "";
    } else {
      bar.classList.remove("indeterminate");
      const pct = Math.min(100, p.done / p.total * 100);
      bar.style.width = pct + "%";
      $("run-percent").textContent = pct.toFixed(1) + "%";
    }
    $("run-rate").textContent = p.rate ? `${fmtBytes(p.rate)}/s` : "—";
    $("run-done").textContent = p.total ? `${fmtBytes(p.done)} / ${fmtBytes(p.total)}` : fmtBytes(p.done);
    $("run-eta").textContent = fmtDuration(p.etaSeconds);
  }
  $("run-elapsed").textContent = fmtDuration((Date.now() - new Date(j.startedAt)) / 1000);

  const log = $("run-log");
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 20;
  log.replaceChildren(...j.log.slice(-100).map((l) =>
    el("li", {}, el("time", {}, new Date(l.time).toLocaleTimeString()), l.msg)));
  if (atBottom) log.scrollTop = log.scrollHeight;
}

// ---------- resume + history ----------

function renderResume() {
  const r = state.status.resumable;
  $("resume").hidden = !r || !!state.status.current;
  if (!r) return;
  const left = r.spec.passes.length - r.passesDone;
  $("resume-text").textContent =
    `${r.spec.volume}: ${r.state} after ${r.passesDone} of ${r.spec.passes.length} pass(es)` +
    (r.error ? ` — ${r.error}` : "") + `. Resuming runs the remaining ${left} pass(es).`;
}

function renderHistory() {
  const h = state.status.history || [];
  $("history-section").hidden = h.length === 0;
  const body = $("history");
  body.replaceChildren();
  for (const j of h) {
    const dur = j.finishedAt ? (new Date(j.finishedAt) - new Date(j.startedAt)) / 1000 : null;
    const passes = `${j.passesDone}/${j.spec.passes.length} · ${j.spec.passes.map(patternLabel).join(", ")}`;
    body.append(el("tr", {},
      el("td", {}, fmtTime(j.startedAt)),
      el("td", {}, j.spec.volume),
      el("td", {}, passes),
      el("td", {}, el("span", { class: `state state-${j.state}` }, j.state)),
      el("td", {}, fmtDuration(dur)),
    ));
    if (j.error) body.append(el("tr", {}, el("td", { class: "err", colspan: 5 }, j.error)));
  }
}

// ---------- polling ----------

function renderAll() {
  renderRunning();
  renderResume();
  renderVolumes();
  renderSetup();
  renderHistory();
}

let lastVolumes = 0;

async function refresh() {
  try {
    const running = !!state.status.current;
    const wantVolumes = Date.now() - lastVolumes > (running ? 5000 : 15000);
    const [status, volumes] = await Promise.all([
      api("GET", "/api/status"),
      wantVolumes ? api("GET", "/api/volumes") : null,
    ]);
    state.status = status;
    if (volumes) { state.volumes = volumes; lastVolumes = Date.now(); }
    if (state.selected && !state.volumes.some((v) => v.name === state.selected)) state.selected = null;
    setOnline(true);
    renderAll();
  } catch (e) {
    setOnline(false);
  }
}

function setOnline(ok) {
  state.online = ok;
  $("conn").hidden = ok;
}

async function loop() {
  await refresh();
  setTimeout(loop, state.status.current ? 1000 : 4000);
}

// ---------- wiring ----------

document.querySelectorAll("[data-preset]").forEach((b) =>
  b.addEventListener("click", () => {
    state.passes = [...PRESETS[b.dataset.preset]];
    renderPasses();
    renderEstimate();
  }));

$("add-pass").addEventListener("click", () => {
  if (state.passes.length < MAX_PASSES) state.passes.push("zero");
  renderPasses();
  renderEstimate();
});
document.querySelectorAll('input[name="mode"]').forEach((r) => r.addEventListener("change", renderEstimate));
$("margin").addEventListener("input", () => {
  document.querySelector('input[name="mode"][value="margin"]').checked = true;
  renderEstimate();
});
$("verify").addEventListener("change", renderEstimate);
$("setup-close").addEventListener("click", () => selectVolume(state.selected));
$("start").addEventListener("click", startWipe);

$("cancel").addEventListener("click", async () => {
  if (!confirm("Cancel the wipe? Fill files will be removed. You can resume the remaining passes later.")) return;
  try { await api("POST", "/api/jobs/cancel"); } catch (e) { showError(e.message); }
  refresh();
});
$("resume-go").addEventListener("click", async () => {
  try { await api("POST", "/api/jobs/resume"); showError(""); } catch (e) { showError(`Could not resume: ${e.message}`); }
  refresh();
});
$("resume-discard").addEventListener("click", async () => {
  try { await api("DELETE", "/api/jobs/resumable"); } catch (e) { showError(e.message); }
  refresh();
});

loop();
