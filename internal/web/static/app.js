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
const PAGE = 100;
// Types browsers can show as a thumbnail.
const THUMB = new Set(["jpg", "png", "gif", "bmp", "webp", "avif"]);
// Types worth opening in a new tab (others download).
const VIEWABLE = new Set([...THUMB, "mp4", "m4v", "webm", "mp3", "wav", "ogg", "opus", "flac", "m4a",
  "txt", "html", "xml", "svg", "json", "eml", "vcf", "ics", "sh", "rtf"]);
const CATEGORY_ORDER = ["Images", "Video", "Audio", "Documents", "Archives", "Databases", "Programs", "Text"];

const state = {
  volumes: [],
  status: { current: null, resumable: null, history: [] },
  selected: null,
  tab: "wipe",
  passes: ["zero"],
  scans: [],
  device: null,      // device info for the selected volume
  destinations: [],
  scan: null,        // open scan summary
  exts: new Set(),   // type filter
  checked: new Set(),// explicitly ticked item numbers
  page: { offset: 0, data: null },
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

function fmtNum(n) { return Number(n || 0).toLocaleString(); }

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k === "style") e.style.cssText = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k === "checked" || k === "indeterminate") e[k] = !!v;
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
  if (msg) box.scrollIntoView({ behavior: "smooth", block: "nearest" });
}

let confirmHandler = null;
function confirmDialog(title, lines, note, okLabel, onOk) {
  $("confirm-title").textContent = title;
  $("confirm-list").replaceChildren(...lines.map((l) => el("li", {}, l)));
  $("confirm-note").textContent = note;
  $("confirm-ok").textContent = okLabel;
  const dlg = $("confirm");
  dlg.returnValue = "";
  confirmHandler = onOk;
  dlg.showModal();
}
$("confirm").addEventListener("close", () => {
  const h = confirmHandler;
  confirmHandler = null;
  if ($("confirm").returnValue === "ok" && h) h();
});

const busy = () => !!state.status.current;

// ---------- volumes ----------

function renderVolumes() {
  const wrap = $("volumes");
  wrap.replaceChildren();
  $("no-volumes").hidden = state.volumes.length > 0;
  const cur = state.status.current;

  for (const v of state.volumes) {
    const usable = !v.error && v.isMount;
    const used = v.total - v.free;
    const pct = (x) => v.total ? (x / v.total * 100).toFixed(2) + "%" : "0";
    const tag = cur && cur.volume === v.name
      ? el("span", { class: "tag" }, { wipe: "Wiping", scan: "Scanning", recover: "Recovering" }[cur.kind] || "Busy")
      : el("span", { class: "vol-meta" }, [v.fsType, v.source].filter(Boolean).join(" · "));

    const card = el("button", {
      class: "vol" + (state.selected === v.name ? " selected" : ""),
      disabled: !usable,
      onclick: () => selectVolume(v.name),
    }, el("div", { class: "vol-head" }, el("span", { class: "vol-name" }, v.name), tag));

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
      if (!v.isMount) card.append(el("div", { class: "vol-warn" }, "Not a mount point — bind-mount a host volume here."));
    }
    wrap.append(card);
  }
}

function selectVolume(name) {
  state.selected = state.selected === name ? null : name;
  state.setupFor = null;
  state.device = null;
  showError("");
  renderVolumes();
  renderPanel();
  if (state.selected) {
    if (state.tab === "scan") loadScanTab();
    $("panel").scrollIntoView({ behavior: "smooth", block: "start" });
  }
}

const selectedVolume = () => state.volumes.find((v) => v.name === state.selected);

// ---------- panel + tabs ----------

function renderPanel() {
  const v = selectedVolume();
  $("panel").hidden = !v;
  if (!v) return;
  $("panel-volume").textContent = v.name;
  document.querySelectorAll(".tab").forEach((t) => t.setAttribute("aria-selected", String(t.dataset.tab === state.tab)));
  $("tab-wipe").hidden = state.tab !== "wipe";
  $("tab-scan").hidden = state.tab !== "scan";
  if (state.tab === "wipe") renderWipe(v); else renderScanTab(v);
}

document.querySelectorAll(".tab").forEach((t) => t.addEventListener("click", () => {
  state.tab = t.dataset.tab;
  renderPanel();
  if (state.tab === "scan") loadScanTab();
}));

// ---------- wipe tab ----------

const mode = () => document.querySelector('input[name="mode"]:checked').value;

function renderWipe(v) {
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
  $("start").disabled = busy() || per <= 0;
  if (per <= 0) {
    box.textContent = "The margin is larger than the free space — nothing would be written.";
    return;
  }
  let text = `Writes about ${fmtBytes(per)} per pass`;
  if (n > 1) text += `, ${fmtBytes(per * n)} in total`;
  if ($("verify").checked) text += `, plus the same amount read back to verify`;
  box.textContent = text + "." + (busy() ? " Another job is running." : "");
}

function startWipe() {
  const v = selectedVolume();
  if (!v) return;
  const spec = {
    volume: v.name,
    passes: [...state.passes],
    mode: mode(),
    marginGB: Math.max(0, Number($("margin").value) || 0),
    verify: $("verify").checked,
  };
  confirmDialog("Start wipe?", [
    `Volume: ${v.name} (${v.path})`,
    `${spec.passes.length} pass${spec.passes.length > 1 ? "es" : ""}: ${spec.passes.map(patternLabel).join(", ")}`,
    spec.mode === "full" ? "Fill completely, including reserved space" : `Leave ${spec.marginGB} GB free`,
    spec.verify ? "Verify each pass" : "No verification",
    `About ${fmtBytes(bytesPerPass(v) * spec.passes.length)} to write`,
  ], "You can cancel at any time; fill files are removed when the job stops.", "Start", async () => {
    try {
      await api("POST", "/api/jobs", spec);
      showError("");
      await refresh();
    } catch (e) { showError(`Could not start: ${e.message}`); }
  });
}

// ---------- scan tab ----------

async function loadScanTab() {
  const name = state.selected;
  if (!name) return;
  try {
    const [device, scans, dests] = await Promise.all([
      api("GET", `/api/volumes/${encodeURIComponent(name)}/device`),
      api("GET", "/api/scans"),
      api("GET", "/api/destinations"),
    ]);
    if (state.selected !== name) return;
    state.device = device;
    state.scans = scans;
    state.destinations = dests;
    renderPanel();
  } catch (e) { showError(e.message); }
}

function renderScanTab(v) {
  const box = $("device");
  const d = state.device;
  box.className = "device";
  if (!d) {
    box.textContent = "Checking device access…";
  } else if (d.device) {
    box.replaceChildren(
      el("span", { class: "ok" }, "✓ Ready. "),
      `Reading ${d.device.path} — ${d.device.via}.`,
    );
  } else {
    box.className = "device bad";
    box.replaceChildren(el("strong", {}, "Can't scan yet. "), d.error || "");
    if (d.hostDev) {
      box.append(
        el("div", { class: "small", style: "margin-top:8px" }, "Add this to the ZeroOut compose file under the service, then redeploy:"),
        el("pre", {}, `    devices:\n      - ${d.hostDev}:${d.hostDev}:r`),
      );
    }
  }
  const free = v.free;
  $("scan-start").disabled = busy() || !(d && d.device);
  $("scan-start").title = `Reads about ${fmtBytes(free)} of free space`;

  const mine = state.scans.filter((s) => s.volume === v.name);
  $("scan-list-wrap").hidden = mine.length === 0;
  $("scan-list").replaceChildren(...mine.map((s) => el("li", { class: state.scan && state.scan.id === s.id ? "current" : "" },
    el("span", {},
      el("strong", {}, fmtTime(s.startedAt)), " · ",
      s.state === "completed" ? `${fmtNum(s.items)} files (${fmtBytes(s.itemBytes)}), ${fmtBytes(s.nonZeroBytes)} not wiped`
        : s.state === "running" ? "running…"
        : `${s.state}${s.error ? ": " + s.error : ""} — ${fmtNum(s.items)} files`,
    ),
    el("span", { class: "actions" },
      s.state !== "running" ? el("button", { class: "btn btn-ghost btn-sm", onclick: () => openScan(s.id) }, "Open") : null,
      s.state !== "running" ? el("button", { class: "btn btn-ghost btn-sm", onclick: () => deleteScan(s.id) }, "Delete") : null,
    ),
  )));
}

function startScan() {
  const v = selectedVolume();
  if (!v) return;
  confirmDialog("Scan for deleted files?", [
    `Volume: ${v.name}`,
    `Reads about ${fmtBytes(v.free)} of free space (read-only)`,
    "At roughly 300–400 MB/s from your RAID, expect " + fmtDuration(v.free / 350e6),
  ], "The volume stays fully usable during the scan.", "Scan", async () => {
    try {
      await api("POST", "/api/scans", { volume: v.name });
      showError("");
      await refresh();
    } catch (e) { showError(`Could not start scan: ${e.message}`); }
  });
}

async function deleteScan(id) {
  if (!confirm("Delete this scan's results? (Nothing on the volume is affected.)")) return;
  try {
    await api("DELETE", `/api/scans/${id}`);
    if (state.scan && state.scan.id === id) closeScan();
    loadScanTab();
  } catch (e) { showError(e.message); }
}

// ---------- results ----------

async function openScan(id) {
  try {
    const s = await api("GET", `/api/scans/${id}`);
    state.scan = s;
    state.exts = new Set(s.kinds.map((k) => k.ext));
    state.checked = new Set();
    state.page = { offset: 0, data: null };
    if (!state.destinations.length) state.destinations = await api("GET", "/api/destinations");
    renderResults();
    renderPanel();
    await loadItems();
    $("results").scrollIntoView({ behavior: "smooth", block: "start" });
  } catch (e) { showError(e.message); }
}

function closeScan() {
  state.scan = null;
  $("results").hidden = true;
  renderPanel();
}

function renderResults() {
  const s = state.scan;
  $("results").hidden = !s;
  if (!s) return;
  $("res-title").textContent = `${s.volume} · ${fmtTime(s.startedAt)}`;
  const warn = $("res-warning");
  warn.hidden = s.state === "completed";
  warn.textContent = s.state === "partial" ? "This scan was cancelled; results cover only the part that was read."
    : s.state === "failed" ? `This scan failed: ${s.error || "unknown error"}. Results are incomplete.` : "";

  const pct = s.scannedBytes ? (s.nonZeroBytes / s.scannedBytes * 100) : 0;
  const stat = (label, value, sub) => el("div", {}, el("dt", {}, label), el("dd", {}, value), sub ? el("div", { class: "muted small" }, sub) : null);
  $("res-stats").replaceChildren(
    stat("Free space read", fmtBytes(s.scannedBytes), s.neverUsedBytes ? `${fmtBytes(s.neverUsedBytes)} never used, skipped` : null),
    stat("Still holds data", fmtBytes(s.nonZeroBytes), `${pct.toFixed(1)}% of free space not wiped`),
    stat("Files found", fmtNum(s.items), fmtBytes(s.itemBytes)),
    stat("Unidentified data", fmtBytes(Math.max(0, s.nonZeroBytes - s.identifiedBytes)), "fragments of unknown type"),
  );

  const heat = $("heat");
  heat.replaceChildren(...(s.heat || []).map((c) => {
    const f = c.free ? c.nonZero / c.free : 0;
    const style = c.free === 0
      ? "background:var(--used);opacity:.35"
      : `background:color-mix(in srgb, var(--danger) ${Math.round(Math.sqrt(f) * 100)}%, var(--surface-2))`;
    return el("span", { style, title: c.free ? `${(f * 100).toFixed(1)}% of free space here still holds data` : "no free space here" });
  }));
  heat.setAttribute("aria-label", `${pct.toFixed(1)} percent of free space still holds data`);

  renderTypes();
  renderDestinations();
}

function renderTypes() {
  const s = state.scan;
  const body = $("types");
  body.replaceChildren();
  const byCat = new Map();
  for (const k of s.kinds) {
    if (!byCat.has(k.category)) byCat.set(k.category, []);
    byCat.get(k.category).push(k);
  }
  const cats = [...byCat.keys()].sort((a, b) => CATEGORY_ORDER.indexOf(a) - CATEGORY_ORDER.indexOf(b));
  for (const cat of cats) {
    const kinds = byCat.get(cat);
    const sel = kinds.filter((k) => state.exts.has(k.ext)).length;
    const sum = (f) => kinds.reduce((a, k) => a + k[f], 0);
    body.append(el("tr", { class: "cat" },
      el("td", {}, el("input", { type: "checkbox", "aria-label": `Select ${cat}`, checked: sel === kinds.length, indeterminate: sel > 0 && sel < kinds.length,
        onchange: (e) => { kinds.forEach((k) => e.target.checked ? state.exts.add(k.ext) : state.exts.delete(k.ext)); filterChanged(); } })),
      el("td", {}, cat),
      el("td", { class: "num-col" }, fmtNum(sum("count"))),
      el("td", { class: "num-col" }, fmtBytes(sum("bytes"))),
      el("td", { class: "num-col" }, fmtNum(sum("exact"))),
      el("td", { class: "num-col" }, fmtNum(sum("truncated"))),
    ));
    for (const k of kinds) {
      body.append(el("tr", {},
        el("td", {}, el("input", { type: "checkbox", "aria-label": `Select ${k.desc}`, checked: state.exts.has(k.ext),
          onchange: (e) => { e.target.checked ? state.exts.add(k.ext) : state.exts.delete(k.ext); filterChanged(); } })),
        el("td", {}, `${k.desc} `, el("span", { class: "muted small" }, `.${k.ext}`)),
        el("td", { class: "num-col" }, fmtNum(k.count)),
        el("td", { class: "num-col" }, fmtBytes(k.bytes)),
        el("td", { class: "num-col" }, fmtNum(k.exact)),
        el("td", { class: "num-col" }, fmtNum(k.truncated)),
      ));
    }
  }
  const all = s.kinds.length > 0 && s.kinds.every((k) => state.exts.has(k.ext));
  $("types-all").checked = all;
  $("types-all").indeterminate = !all && state.exts.size > 0;
}

function query(offset = state.page.offset) {
  const s = state.scan;
  const q = new URLSearchParams({
    offset, limit: PAGE,
    minSize: Math.max(0, Number($("f-min").value) || 0) * 1000,
    status: $("f-status").value,
    sort: $("f-sort").value,
  });
  // An empty selection must match nothing, not everything.
  const exts = s.kinds.map((k) => k.ext).filter((e) => state.exts.has(e));
  q.set("exts", exts.length ? exts.join(",") : "none");
  return q;
}

async function loadItems() {
  const s = state.scan;
  if (!s) return;
  try {
    const data = await api("GET", `/api/scans/${s.id}/items?${query()}`);
    if (state.scan !== s) return;
    state.page.data = data;
    renderItems();
  } catch (e) { showError(e.message); }
}

function filterChanged() {
  state.page.offset = 0;
  state.checked.clear();
  renderTypes();
  loadItems();
}

function contentURL(n, inline) {
  return `/api/scans/${state.scan.id}/items/${n}/content${inline ? "?inline=1" : ""}`;
}

function renderItems() {
  const { data, offset } = state.page;
  const body = $("items");
  body.replaceChildren();
  for (const it of data.items) {
    const thumb = THUMB.has(it.ext)
      ? el("img", { loading: "lazy", alt: "", src: contentURL(it.n, true), onerror: (e) => { e.target.replaceWith(el("div", { class: "ext" }, "gone")); } })
      : el("div", { class: "ext" }, it.ext);
    const status = it.truncated ? el("span", { class: "badge partial" }, "partial")
      : it.exact ? el("span", { class: "badge complete" }, "complete")
      : el("span", { class: "badge" }, "estimated");
    body.append(el("tr", {},
      el("td", {}, el("input", { type: "checkbox", "aria-label": `Select item ${it.n}`, checked: state.checked.has(it.n),
        onchange: (e) => { e.target.checked ? state.checked.add(it.n) : state.checked.delete(it.n); renderRecoverButton(); } })),
      el("td", { class: "thumb" }, thumb),
      el("td", {}, it.ext.toUpperCase(), " ", el("span", { class: "muted small" }, it.category)),
      el("td", { class: "num-col" }, fmtBytes(it.size)),
      el("td", { class: "muted small" }, `${fmtBytes(it.offset)} in`),
      el("td", {}, status),
      el("td", {},
        VIEWABLE.has(it.ext) ? el("a", { href: contentURL(it.n, true), target: "_blank", rel: "noopener" }, "Open") : null,
        el("a", { href: contentURL(it.n, false) }, "Download"),
      ),
    ));
  }
  if (!data.items.length) body.append(el("tr", {}, el("td", { colspan: 7, class: "muted" }, "Nothing matches these filters.")));
  $("f-count").textContent = `${fmtNum(data.total)} files, ${fmtBytes(data.totalBytes)}`;
  $("pg-info").textContent = data.total ? `${fmtNum(offset + 1)}–${fmtNum(Math.min(offset + PAGE, data.total))} of ${fmtNum(data.total)}` : "";
  $("pg-prev").disabled = offset === 0;
  $("pg-next").disabled = offset + PAGE >= data.total;
  $("items-page").checked = data.items.length > 0 && data.items.every((it) => state.checked.has(it.n));
  renderRecoverButton();
}

function renderDestinations() {
  const sel = $("dest");
  const prev = sel.value;
  const vol = state.scan ? state.scan.volume : "";
  sel.replaceChildren(...state.destinations.map((d) => {
    const same = d.sameDeviceAs.includes(vol);
    return el("option", { value: d.name, disabled: same || !!d.error, selected: d.name === prev },
      `${d.name} — ${fmtBytes(d.avail)} free${same ? " (same disk, not allowed)" : ""}`);
  }));
  if (!state.destinations.length) sel.append(el("option", { value: "", disabled: true, selected: true }, "none configured"));
  $("dest-note").textContent = state.destinations.length ? "" : "Mount a destination under /recover in the compose file.";
  renderRecoverButton();
}

function recoverSelection() {
  const data = state.page.data;
  if (state.checked.size) return { label: `${fmtNum(state.checked.size)} selected file${state.checked.size > 1 ? "s" : ""}`, items: [...state.checked] };
  if (!data) return null;
  return { label: `all ${fmtNum(data.total)} matching files (${fmtBytes(data.totalBytes)})`, total: data.total };
}

function renderRecoverButton() {
  const sel = recoverSelection();
  const btn = $("recover");
  btn.textContent = sel ? `Recover ${sel.label}…` : "Recover…";
  btn.disabled = busy() || !sel || (!sel.items && !sel.total) || !$("dest").value;
}

function startRecover() {
  const sel = recoverSelection();
  const dest = state.destinations.find((d) => d.name === $("dest").value);
  if (!sel || !dest) return;
  const q = query(0);
  const spec = {
    scanId: state.scan.id,
    destination: dest.name,
    items: sel.items,
    query: {
      exts: q.get("exts").split(","),
      minSize: Number(q.get("minSize")),
      status: q.get("status"),
      sort: q.get("sort"),
    },
  };
  confirmDialog("Recover files?", [
    `Recover ${sel.label}`,
    `To ${dest.name} (${fmtBytes(dest.avail)} free), folder ZeroOut-Recovered/${state.scan.id}`,
    "Files whose space was reused since the scan are skipped",
  ], "Each file is checked again before copying; a manifest.csv lists what was written.", "Recover", async () => {
    try {
      await api("POST", "/api/recover", spec);
      showError("");
      state.checked.clear();
      await refresh();
    } catch (e) { showError(`Could not start recovery: ${e.message}`); }
  });
}

// ---------- running job ----------

const PHASE_LABEL = { writing: "Writing", verifying: "Verifying", cleaning: "Removing fill files", scanning: "Reading free space", recovering: "Copying files" };

function renderRunning() {
  const j = state.status.current;
  $("running").hidden = !j;
  if (!j) return;
  const p = j.progress;
  const wipe = j.kind === "wipe";
  $("run-eyebrow").textContent = { wipe: "Wiping", scan: "Scanning for deleted files", recover: "Recovering files" }[j.kind];
  $("run-volume").textContent = j.kind === "recover" && j.recover
    ? `${j.volume} → ${j.recover.destination}` : j.volume;

  const pills = $("run-passes");
  pills.hidden = !wipe;
  pills.replaceChildren();
  if (wipe && j.spec) {
    j.spec.passes.forEach((pat, i) => {
      const cls = i < j.passesDone ? "done" : p && p.pass - 1 === i ? "current" : "";
      pills.append(el("li", { class: cls }, `${i + 1}. ${patternLabel(pat)}`));
    });
  }

  $("run-found-wrap").hidden = wipe;
  $("run-found-label").textContent = j.kind === "recover" ? "Files written" : "Files found";
  $("run-found").textContent = fmtNum(j.found);
  $("run-done-label").textContent = wipe ? "This phase" : j.kind === "scan" ? "Read" : "Copied";

  const bar = $("run-bar");
  if (!p) {
    $("run-phase").textContent = "Starting…";
    $("run-percent").textContent = "";
    bar.classList.add("indeterminate");
  } else {
    const label = PHASE_LABEL[p.phase] || p.phase;
    $("run-phase").textContent = wipe ? `Pass ${p.pass} of ${p.passes} · ${label}` : label;
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
  $("resume").hidden = !r || busy();
  if (!r || !r.spec) return;
  const left = r.spec.passes.length - r.passesDone;
  $("resume-text").textContent =
    `${r.volume}: ${r.state} after ${r.passesDone} of ${r.spec.passes.length} pass(es)` +
    (r.error ? ` — ${r.error}` : "") + `. Resuming runs the remaining ${left} pass(es).`;
}

function jobDetails(j) {
  switch (j.kind) {
    case "scan": return `${fmtNum(j.found)} files found`;
    case "recover": {
      const r = j.recover || {};
      let s = `${fmtNum(r.written)} written to ${r.destination}`;
      if (r.overwritten) s += `, ${fmtNum(r.overwritten)} already overwritten`;
      if (r.errors) s += `, ${fmtNum(r.errors)} errors`;
      return s;
    }
    default: return j.spec ? `${j.passesDone}/${j.spec.passes.length} · ${j.spec.passes.map(patternLabel).join(", ")}` : "";
  }
}

function renderHistory() {
  const h = state.status.history || [];
  $("history-section").hidden = h.length === 0;
  const body = $("history");
  body.replaceChildren();
  for (const j of h) {
    const dur = j.finishedAt ? (new Date(j.finishedAt) - new Date(j.startedAt)) / 1000 : null;
    body.append(el("tr", {},
      el("td", {}, fmtTime(j.startedAt)),
      el("td", {}, { wipe: "Wipe", scan: "Scan", recover: "Recover" }[j.kind] || j.kind),
      el("td", {}, j.volume),
      el("td", {}, jobDetails(j)),
      el("td", {}, el("span", { class: `state state-${j.state}` }, j.state)),
      el("td", {}, fmtDuration(dur)),
    ));
    if (j.error) body.append(el("tr", {}, el("td", { class: "err", colspan: 6 }, j.error)));
  }
}

// ---------- polling ----------

function renderAll() {
  renderRunning();
  renderResume();
  renderVolumes();
  renderPanel();
  renderHistory();
  if (state.scan) renderRecoverButton();
}

let lastVolumes = 0;

async function refresh() {
  try {
    const before = state.status.current;
    const wantVolumes = Date.now() - lastVolumes > (busy() ? 5000 : 15000);
    const [status, volumes] = await Promise.all([
      api("GET", "/api/status"),
      wantVolumes ? api("GET", "/api/volumes") : null,
    ]);
    state.status = status;
    if (volumes) { state.volumes = volumes; lastVolumes = Date.now(); }
    if (state.selected && !state.volumes.some((v) => v.name === state.selected)) state.selected = null;
    setOnline(true);
    renderAll();
    // A job just finished: refresh what it changed.
    if (before && !status.current) {
      lastVolumes = 0;
      if (before.kind === "scan") {
        await loadScanTab();
        if (state.selected === before.volume) openScan(before.scanId);
      } else if (before.kind === "recover") {
        state.destinations = await api("GET", "/api/destinations");
        if (state.scan) { renderDestinations(); loadItems(); }
      }
    }
  } catch (e) {
    setOnline(false);
  }
}

function setOnline(ok) { $("conn").hidden = ok; }

async function loop() {
  await refresh();
  setTimeout(loop, busy() ? 1000 : 4000);
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
$("start").addEventListener("click", startWipe);
$("scan-start").addEventListener("click", startScan);
$("res-close").addEventListener("click", closeScan);
$("recover").addEventListener("click", startRecover);
$("dest").addEventListener("change", renderRecoverButton);

$("types-all").addEventListener("change", (e) => {
  state.exts = e.target.checked ? new Set(state.scan.kinds.map((k) => k.ext)) : new Set();
  filterChanged();
});
$("items-page").addEventListener("change", (e) => {
  for (const it of state.page.data.items) e.target.checked ? state.checked.add(it.n) : state.checked.delete(it.n);
  renderItems();
});
let minTimer;
$("f-min").addEventListener("input", () => { clearTimeout(minTimer); minTimer = setTimeout(filterChanged, 300); });
$("f-status").addEventListener("change", filterChanged);
$("f-sort").addEventListener("change", filterChanged);
$("pg-prev").addEventListener("click", () => { state.page.offset = Math.max(0, state.page.offset - PAGE); loadItems(); });
$("pg-next").addEventListener("click", () => { state.page.offset += PAGE; loadItems(); });

$("cancel").addEventListener("click", async () => {
  const j = state.status.current;
  const msg = j && j.kind === "wipe"
    ? "Cancel the wipe? Fill files will be removed. You can resume the remaining passes later."
    : "Cancel this job? Results so far are kept.";
  if (!confirm(msg)) return;
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
