/* GlucoGuard web app.
 * Every safety decision comes from the Go engine compiled to WebAssembly
 * (window.glucoguard.evaluate). This file only stores readings on the device
 * and renders the decisions. It never decides clinical outcomes itself.
 */
"use strict";

const STORE_KEY = "glucoguard.v1";
const ACK_VERSION = "2026-10-draft";
const MGDL_PER_MMOL = 18.0182;

const SYMPTOMS = {
  emergency: [
    ["unconscious", "Passed out / can't be woken"],
    ["seizure", "Seizure / fit"],
    ["unable_to_swallow", "Can't swallow safely"],
  ],
  warning: [
    ["vomiting", "Vomiting"],
    ["abdominal_pain", "Stomach pain"],
    ["difficulty_breathing", "Hard to breathe"],
    ["confusion", "Confused or drowsy"],
  ],
  low: [
    ["shaky", "Shaky"], ["sweating", "Sweaty"], ["dizzy", "Dizzy"], ["hungry", "Very hungry"],
  ],
  high: [
    ["extreme_thirst", "Very thirsty"], ["frequent_urination", "Peeing a lot"], ["blurred_vision", "Blurred vision"],
  ],
};
const SYMPTOM_LABEL = Object.fromEntries(Object.values(SYMPTOMS).flat());
const MEALS = [
  ["unspecified", "Not sure"], ["fasting", "Fasting"], ["before_meal", "Before a meal"],
  ["after_meal", "After a meal"], ["bedtime", "Bedtime"], ["overnight", "Overnight"],
];
const URGENCY_LABEL = {
  info: "All good", monitor: "Keep an eye on it", action: "Do this now", urgent: "Act now", emergency: "Emergency",
};

let META = null;
let state = load();
let tab = "today";
let tickTimer = null;

/* ---------------- storage ---------------- */

function defaults() {
  return {
    ack: null,
    profile: { age_group: "adult", diabetes_type: "type2", pregnant: false, special_circumstances: false, unit: "mg/dL" },
    plan: null,          // targets entered from the patient's doctor
    readings: [],        // newest last
    notifyAsked: false,
  };
}
function load() {
  try {
    const raw = localStorage.getItem(STORE_KEY);
    if (raw) return Object.assign(defaults(), JSON.parse(raw));
  } catch (_) { /* storage blocked or corrupt */ }
  return defaults();
}
function save() {
  try { localStorage.setItem(STORE_KEY, JSON.stringify(state)); }
  catch (_) { toast("Couldn't save on this device. Check that site storage is allowed."); }
}

/* ---------------- engine ---------------- */

function enginePlan() {
  const p = state.plan;
  if (!p) return null;
  return {
    version: p.version, low_mg_dl: +p.low_mg_dl, severe_low_mg_dl: +p.severe_low_mg_dl,
    high_mg_dl: +p.high_mg_dl, very_high_mg_dl: +p.very_high_mg_dl,
    stale_after_minutes: 15, low_recheck_after_minutes: 15, high_recheck_after_minutes: 120,
    max_low_treatment_cycles: 2, clinician_approved: !!p.from_doctor, approved_by: p.doctor || "",
  };
}

function priorFor(reading) {
  const t = Date.parse(reading.measured_at);
  let best = null;
  for (const r of state.readings) {
    if (r.id === reading.id || !r.decision) continue;
    const rt = Date.parse(r.measured_at);
    if (rt < t && (!best || rt > Date.parse(best.measured_at))) best = r;
  }
  if (!best) return null;
  return {
    action_code: best.decision.action_code, cycle: best.decision.low_treatment_cycle || 0,
    value_mg_dl: best.decision.value_mg_dl, measured_at: best.measured_at,
  };
}

function evaluate(reading, extraSymptoms = []) {
  const symptoms = [...new Set([...(reading.symptoms || []), ...extraSymptoms])];
  const input = {
    reading: {
      value: reading.value, unit: reading.unit, meter_flag: reading.meter_flag || "",
      source: "manual_meter", measured_at: reading.measured_at, received_at: reading.received_at,
    },
    symptoms,
    patient: {
      age_group: state.profile.age_group, diabetes_type: state.profile.diabetes_type,
      pregnant: !!state.profile.pregnant, special_circumstances: !!state.profile.special_circumstances,
    },
    plan: enginePlan(),
    prior: priorFor(reading),
    now: new Date().toISOString(),
  };
  const res = window.glucoguard.evaluate(JSON.stringify(input));
  if (res.error) throw new Error(res.error);
  return JSON.parse(res.ok);
}

/* ---------------- helpers ---------------- */

const $ = (sel, root = document) => root.querySelector(sel);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

function fmtValue(mg, unit = state.profile.unit) {
  if (unit === "mmol/L") return (mg / MGDL_PER_MMOL).toFixed(1);
  return String(Math.round(mg));
}
function readingText(r) {
  if (r.meter_flag === "LO") return "LO";
  if (r.meter_flag === "HI") return "HI";
  return fmtValue(r.decision ? r.decision.value_mg_dl : r.value);
}
function ago(iso) {
  const m = Math.round((Date.now() - Date.parse(iso)) / 60000);
  if (m < 1) return "just now";
  if (m < 60) return `${m} min ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} h ${m % 60} min ago`;
  return new Date(iso).toLocaleString([], { day: "numeric", month: "short", hour: "numeric", minute: "2-digit" });
}
function timeOf(iso) { return new Date(iso).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }); }
function localInputValue(d = new Date()) {
  const off = d.getTimezoneOffset();
  return new Date(d.getTime() - off * 60000).toISOString().slice(0, 16);
}
function uid() { return (crypto.randomUUID ? crypto.randomUUID() : String(Date.now()) + Math.random().toString(16).slice(2)); }
function latest() { return state.readings.length ? state.readings[state.readings.length - 1] : null; }
function sortReadings() { state.readings.sort((a, b) => Date.parse(a.measured_at) - Date.parse(b.measured_at)); }

function toast(msg) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.add("show");
  clearTimeout(toast._t);
  toast._t = setTimeout(() => t.classList.remove("show"), 2800);
}

/* ---------------- rendering ---------------- */

function render() {
  clearInterval(tickTimer);
  const app = $("#app");
  document.querySelectorAll(".tabs button").forEach((b) => {
    if (b.dataset.tab === tab) b.setAttribute("aria-current", "page"); else b.removeAttribute("aria-current");
  });
  $(".tabs").hidden = !state.ack;
  if (!state.ack) { app.innerHTML = gateView(); bindGate(); return; }
  if (tab === "today") { app.innerHTML = todayView(); bindToday(); }
  if (tab === "history") { app.innerHTML = historyView(); bindHistory(); }
  if (tab === "plan") { app.innerHTML = planView(); bindPlan(); }
}

/* ---- first-run gate ---- */

function gateView() {
  return `
  <section class="gate">
    <h1>Before you start</h1>
    <p>GlucoGuard lets you log blood glucose readings and shows what to do next, based on fixed safety rules.</p>
    <ul>
      <li><strong>This is a preview.</strong> The rules and messages are drafts that a diabetes doctor has not yet reviewed. Don't use this app instead of advice from your doctor.</li>
      <li>It never suggests insulin doses or changes to your medicine.</li>
      <li>In an emergency, call <a href="tel:112">112</a>. Don't wait for the app.</li>
      <li>Your readings are stored only on this phone. Nothing is sent to a server.</li>
    </ul>
    <label class="check"><input type="checkbox" id="ack"> <span>I understand this is a preview and not medical advice.</span></label>
    <button class="btn btn-primary" id="start" disabled>Start</button>
  </section>`;
}
function bindGate() {
  const ack = $("#ack"), start = $("#start");
  ack.addEventListener("change", () => (start.disabled = !ack.checked));
  start.addEventListener("click", () => { state.ack = { version: ACK_VERSION, at: new Date().toISOString() }; save(); render(); });
}

/* ---- today ---- */

function bandFor(r) {
  const d = r.decision;
  const msg = META.messages[d.approved_message_id] || { title: "Reading saved", body: "" };
  const stale = d.data_status !== "current";
  const u = stale && d.urgency !== "urgent" ? "stale" : d.urgency;
  const level = stale ? (d.data_status === "stale" ? "Out of date" : "Check device time") : URGENCY_LABEL[d.urgency];
  const fu = d.follow_up_policy;
  let recheck = "";
  if (fu && fu.required) {
    if (fu.recheck_after_minutes === 0) {
      recheck = `<div class="recheck"><span class="count">Check again now.</span>
        <button class="btn btn-secondary" data-act="log-recheck">Log a new reading</button></div>`;
    } else if (r.recheck_due) {
      recheck = `<div class="recheck"><span class="count" id="countdown"></span>
        <button class="btn btn-secondary" data-act="log-recheck">Log recheck</button>
        <button class="btn btn-quiet" data-act="ics">Add to calendar</button></div>`;
    }
  }
  const esc_ = [];
  if (d.escalation.notify_caregiver) esc_.push("Tell someone near you or your caregiver.");
  if (d.escalation.contact_care_team) esc_.push("Contact your doctor or care team.");
  return `
  <section class="band ${stale ? "stale" : ""}" data-u="${u}" aria-labelledby="band-title">
    <div class="band-top"><span class="level">${esc(level)}</span><span class="when">${esc(ago(r.measured_at))}</span></div>
    <div class="reading" aria-label="Reading ${esc(readingText(r))} ${esc(state.profile.unit)}">
      <span class="num">${esc(readingText(r))}</span><span class="unit">${esc(state.profile.unit)}</span>
    </div>
    <h1 id="band-title">${esc(msg.title)}</h1>
    <p class="body">${esc(msg.body)}</p>
    ${esc_.length ? `<p class="body"><strong>${esc(esc_.join(" "))}</strong></p>` : ""}
    ${recheck}
    <details class="why"><summary>Why am I seeing this?</summary>
      <p>Rules matched: <code>${esc(d.matched_rules.join(", "))}</code>. Targets: ${d.care_plan_approved ? "from your doctor" : "general defaults (add your doctor's targets in My plan)"}.
      Rule set ${esc(d.rule_set_version)}, not yet reviewed by a doctor.</p>
    </details>
  </section>`;
}

function todayView() {
  const r = latest();
  let top;
  if (!r) {
    top = `<section class="empty"><h1>Log your first reading</h1>
      <p class="muted">Enter the number from your glucometer. You'll see what to do next straight away.</p></section>`;
  } else {
    // Re-check the latest reading against the current time, so an old
    // reading is never shown as if it were current.
    try {
      const fresh = evaluate(r, r.extra_symptoms || []);
      if (fresh.data_status !== r.decision.data_status) { r.decision = fresh; save(); }
    } catch (e) { console.error(e); }
    top = bandFor(r);
  }
  return `
    ${top}
    <div class="stack" style="margin-top:16px">
      <button class="btn btn-primary" data-act="log">Log a reading</button>
      ${r ? `<button class="btn btn-quiet" style="width:100%" data-act="symptoms">I feel unwell</button>` : ""}
    </div>
    ${r ? "" : `<p class="notice"><strong>Preview.</strong> Draft safety rules, not yet reviewed by a doctor. In an emergency call 112.</p>`}`;
}

function bindToday() {
  const r = latest();
  if (r && r.recheck_due) {
    const tick = () => {
      const el = $("#countdown");
      if (!el) return;
      const ms = Date.parse(r.recheck_due) - Date.now();
      if (ms <= 0) { el.textContent = "Time to check again."; return; }
      const m = Math.floor(ms / 60000), s = Math.floor((ms % 60000) / 1000);
      el.textContent = `Check again in ${m}:${String(s).padStart(2, "0")}`;
    };
    tick();
    tickTimer = setInterval(tick, 1000);
  }
}
// One delegated click handler for every [data-act] button in #app.
function onAppClick(e) {
  const act = e.target.closest("[data-act]")?.dataset.act;
  if (!act) return;
  if (act === "log" || act === "log-recheck") openLogSheet();
  if (act === "symptoms") openSymptomSheet();
  if (act === "ics") downloadICS(latest());
  if (act === "csv") downloadCSV();
}

/* ---- log sheet ---- */

function chipGroup(list, selected, cls = "") {
  return `<div class="chips">${list.map(([k, l]) =>
    `<button type="button" class="chip ${cls}" data-sym="${k}" aria-pressed="${selected.includes(k)}">${esc(l)}</button>`).join("")}</div>`;
}

function symptomFieldsets(selected = []) {
  return `
    <fieldset><legend>Emergency signs</legend>${chipGroup(SYMPTOMS.emergency, selected, "danger")}</fieldset>
    <fieldset><legend>Warning signs</legend>${chipGroup(SYMPTOMS.warning, selected, "danger")}</fieldset>
    <fieldset><legend>Signs of low glucose</legend>${chipGroup(SYMPTOMS.low, selected)}</fieldset>
    <fieldset><legend>Signs of high glucose</legend>${chipGroup(SYMPTOMS.high, selected)}</fieldset>`;
}

function openSheet(title, inner, onBind) {
  const root = $("#sheet-root");
  root.innerHTML = `<div class="scrim"><div class="sheet" role="dialog" aria-modal="true" aria-labelledby="sheet-title">
    <div class="sheet-head"><h2 id="sheet-title">${esc(title)}</h2><button class="x" data-close>Close</button></div>
    ${inner}</div></div>`;
  const scrim = $(".scrim", root);
  const close = () => { root.innerHTML = ""; document.removeEventListener("keydown", onKey); };
  const onKey = (e) => { if (e.key === "Escape") close(); };
  document.addEventListener("keydown", onKey);
  scrim.addEventListener("click", (e) => { if (e.target === scrim || e.target.hasAttribute("data-close")) close(); });
  scrim.querySelectorAll(".chip").forEach((c) =>
    c.addEventListener("click", () => c.setAttribute("aria-pressed", c.getAttribute("aria-pressed") !== "true")));
  onBind(scrim, close);
  setTimeout(() => (scrim.querySelector("input,select,button.chip") || scrim.querySelector("button"))?.focus(), 50);
}
const selectedSymptoms = (root) => [...root.querySelectorAll('.chip[aria-pressed="true"]')].map((c) => c.dataset.sym);

function openLogSheet() {
  let unit = state.profile.unit;
  let flag = "";
  openSheet("Log a reading", `
    <form id="logf" novalidate>
      <label class="field"><span>Reading</span>
        <input class="big" id="val" type="number" inputmode="decimal" step="any" min="0" placeholder="${unit === "mmol/L" ? "5.6" : "100"}" aria-describedby="valhelp">
        <small id="valhelp">If your meter shows LO or HI, tap the button below instead.</small>
      </label>
      <div class="row" style="margin:-6px 0 16px">
        <div class="seg" role="group" aria-label="Unit" style="flex:2">
          <button type="button" data-unit="mg/dL" aria-pressed="${unit === "mg/dL"}">mg/dL</button>
          <button type="button" data-unit="mmol/L" aria-pressed="${unit === "mmol/L"}">mmol/L</button>
        </div>
        <div class="seg" role="group" aria-label="Meter shows" style="flex:1">
          <button type="button" data-flag="LO" aria-pressed="false">LO</button>
          <button type="button" data-flag="HI" aria-pressed="false">HI</button>
        </div>
      </div>
      <label class="field"><span>When</span><input id="when" type="datetime-local" value="${localInputValue()}" max="${localInputValue(new Date(Date.now() + 5 * 60000))}"></label>
      <label class="field"><span>Meal</span><select id="meal">${MEALS.map(([k, l]) => `<option value="${k}">${esc(l)}</option>`).join("")}</select></label>
      <p style="font-weight:700;margin-bottom:8px">How do you feel? <span class="muted small" style="font-weight:400">Tap any that apply.</span></p>
      ${symptomFieldsets()}
      <p class="error" id="err" role="alert"></p>
      <button class="btn btn-primary" type="submit">Save and check</button>
    </form>`, (root, close) => {
    const val = $("#val", root);
    root.querySelectorAll("[data-unit]").forEach((b) => b.addEventListener("click", () => {
      unit = b.dataset.unit;
      root.querySelectorAll("[data-unit]").forEach((x) => x.setAttribute("aria-pressed", x === b));
      val.placeholder = unit === "mmol/L" ? "5.6" : "100";
    }));
    root.querySelectorAll("[data-flag]").forEach((b) => b.addEventListener("click", () => {
      flag = flag === b.dataset.flag ? "" : b.dataset.flag;
      root.querySelectorAll("[data-flag]").forEach((x) => x.setAttribute("aria-pressed", x.dataset.flag === flag));
      val.disabled = !!flag;
      if (flag) val.value = "";
    }));
    $("#logf", root).addEventListener("submit", (e) => {
      e.preventDefault();
      const err = $("#err", root);
      const v = parseFloat(val.value);
      if (!flag && !(v > 0)) { err.textContent = "Enter the number from your meter, or choose LO or HI."; val.focus(); return; }
      const when = $("#when", root).value ? new Date($("#when", root).value) : new Date();
      const now = new Date();
      const r = {
        id: uid(), value: flag ? 0 : v, unit, meter_flag: flag, meal: $("#meal", root).value,
        symptoms: selectedSymptoms(root), measured_at: when.toISOString(), received_at: now.toISOString(),
      };
      try {
        r.decision = evaluate(r);
      } catch (ex) {
        err.textContent = ex.message.replace(/^invalid input: /, "") + ".";
        return;
      }
      setRecheck(r);
      state.readings.push(r);
      sortReadings();
      save();
      close();
      tab = "today";
      render();
      afterDecision(r);
    });
  });
}

function openSymptomSheet() {
  const r = latest();
  openSheet("How do you feel now?", `
    <p class="muted">We'll check your symptoms together with your last reading (${esc(readingText(r))} ${esc(state.profile.unit)}, ${esc(ago(r.measured_at))}).</p>
    ${symptomFieldsets(r.extra_symptoms || [])}
    <button class="btn btn-primary" id="symok">Check my symptoms</button>`, (root, close) => {
    $("#symok", root).addEventListener("click", () => {
      const extra = selectedSymptoms(root);
      if (!extra.length) { toast("Tap at least one symptom."); return; }
      r.extra_symptoms = extra;
      r.decision = evaluate(r, extra);
      setRecheck(r);
      save();
      close();
      render();
      afterDecision(r);
    });
  });
}

function setRecheck(r) {
  const fu = r.decision.follow_up_policy;
  r.recheck_due = fu && fu.required && fu.recheck_after_minutes > 0
    ? new Date(Date.now() + fu.recheck_after_minutes * 60000).toISOString() : null;
  scheduleReminder();
}

function afterDecision(r) {
  if (r.decision.urgency === "emergency") showEmergency(r);
  else if (r.recheck_due && !state.notifyAsked && "Notification" in window && Notification.permission === "default") {
    state.notifyAsked = true; save();
    Notification.requestPermission().then(scheduleReminder);
  }
}

/* ---- emergency takeover ---- */

function showEmergency(r) {
  const msg = META.messages[r.decision.approved_message_id];
  const root = $("#sheet-root");
  root.innerHTML = `<div class="emergency" role="alertdialog" aria-modal="true" aria-labelledby="em-t" aria-describedby="em-b">
    <h1 id="em-t">${esc(msg.title)}</h1>
    <p id="em-b">${esc(msg.body)}</p>
    <a class="callbig" href="tel:112">Call 112</a>
    <div class="spacer"></div>
    <button class="btn" id="em-x">Close this screen</button>
  </div>`;
  $(".callbig", root).focus();
  $("#em-x", root).addEventListener("click", () => (root.innerHTML = ""));
}

/* ---- reminders ---- */

let reminderTimer = null;
function scheduleReminder() {
  clearTimeout(reminderTimer);
  const r = latest();
  if (!r || !r.recheck_due) return;
  const ms = Date.parse(r.recheck_due) - Date.now();
  if (ms < 0 || ms > 6 * 3600e3) return;
  reminderTimer = setTimeout(async () => {
    toast("Time to check your glucose again.");
    if ("Notification" in window && Notification.permission === "granted") {
      const reg = await navigator.serviceWorker?.getRegistration();
      const opts = { body: "Your recheck is due. Open GlucoGuard to log it.", icon: "icon-192.png", tag: "recheck", requireInteraction: true };
      if (reg) reg.showNotification("Check your glucose again", opts); else new Notification("Check your glucose again", opts);
    }
    if (tab === "today") render();
  }, ms);
}

function downloadICS(r) {
  if (!r?.recheck_due) return;
  const d = new Date(r.recheck_due);
  const f = (x) => x.toISOString().replace(/[-:]/g, "").replace(/\.\d{3}/, "");
  const ics = ["BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//GlucoGuard//EN", "BEGIN:VEVENT",
    `UID:${r.id}@glucoguard`, `DTSTAMP:${f(new Date())}`, `DTSTART:${f(d)}`, `DTEND:${f(new Date(d.getTime() + 5 * 60000))}`,
    "SUMMARY:Check your glucose again", "BEGIN:VALARM", "ACTION:DISPLAY", "DESCRIPTION:Glucose recheck", "TRIGGER:PT0M", "END:VALARM",
    "END:VEVENT", "END:VCALENDAR"].join("\r\n");
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([ics], { type: "text/calendar" }));
  a.download = "glucose-recheck.ics";
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

/* ---- history ---- */

function bounds() {
  const p = state.plan || META.default_plan;
  return { low: +p.low_mg_dl, severe: +p.severe_low_mg_dl, high: state.plan ? +p.high_mg_dl : 180 };
}

function historyView() {
  const rs = state.readings.filter((r) => r.decision);
  if (!rs.length) return `<h2>History</h2><section class="empty"><h1>No readings yet</h1><p class="muted">Readings you log will appear here with a 14-day chart.</p>
    <button class="btn btn-primary" data-act="log">Log a reading</button></section>`;
  const since = Date.now() - 14 * 864e5;
  const recent = rs.filter((r) => Date.parse(r.measured_at) >= since);
  const b = bounds();
  const vals = recent.map((r) => r.decision.value_mg_dl);
  const n = vals.length;
  const inRange = vals.filter((v) => v >= b.low && v <= b.high).length;
  const lows = vals.filter((v) => v < b.low).length;
  const avg = n ? vals.reduce((a, v) => a + v, 0) / n : 0;

  const byDay = new Map();
  [...rs].reverse().forEach((r) => {
    const k = new Date(r.measured_at).toLocaleDateString([], { weekday: "long", day: "numeric", month: "short" });
    if (!byDay.has(k)) byDay.set(k, []);
    byDay.get(k).push(r);
  });
  const list = [...byDay].slice(0, 30).map(([day, items]) => `<div class="day"><h3>${esc(day)}</h3>${items.map((r) => {
    const m = META.messages[r.decision.approved_message_id];
    return `<div class="entry" data-u="${r.decision.urgency}"><time>${esc(timeOf(r.measured_at))}</time>
      <div><div class="v">${esc(readingText(r))} <small>${esc(state.profile.unit)}</small></div><div class="what">${esc(m ? m.title : "")}</div></div>
      <div class="tag">${esc((MEALS.find((x) => x[0] === r.meal) || ["", ""])[1] === "Not sure" ? "" : (MEALS.find((x) => x[0] === r.meal) || ["", ""])[1])}</div></div>`;
  }).join("")}</div>`).join("");

  return `
    <h2>Last 14 days</h2>
    <div class="stats">
      <div class="stat"><b>${n}</b><span>readings</span></div>
      <div class="stat"><b>${n ? Math.round((100 * inRange) / n) : 0}%</b><span>in target range</span></div>
      <div class="stat"><b>${lows}</b><span>below ${esc(fmtValue(b.low))}</span></div>
      <div class="stat"><b>${n ? esc(fmtValue(avg)) : "–"}</b><span>average, ${esc(state.profile.unit)}</span></div>
    </div>
    <div class="chart">${chartSVG(recent, b)}</div>
    <p class="muted small" style="margin-top:8px">Shaded band is your target range (${esc(fmtValue(b.low))}–${esc(fmtValue(b.high))} ${esc(state.profile.unit)}). Percentages count readings, not time.</p>
    <div class="row" style="margin-top:8px"><button class="btn btn-quiet" data-act="csv">Download for my doctor (CSV)</button></div>
    ${list}`;
}

function chartSVG(rs, b) {
  const W = 540, H = 220, L = 40, R = 10, T = 10, B = 26;
  const maxV = Math.max(300, ...rs.map((r) => r.decision.value_mg_dl)) + 10, minV = 30;
  const now = Date.now(), start = now - 14 * 864e5;
  const x = (t) => L + ((t - start) / (now - start)) * (W - L - R);
  const y = (v) => T + (1 - (Math.min(v, maxV) - minV) / (maxV - minV)) * (H - T - B);
  const css = getComputedStyle(document.documentElement);
  const c = (n) => css.getPropertyValue(n).trim();
  const ticks = [b.severe, b.low, b.high, 250].filter((v, i, a) => a.indexOf(v) === i && v < maxV);
  let s = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Glucose readings over the last 14 days">`;
  s += `<rect x="${L}" y="${y(b.high)}" width="${W - L - R}" height="${y(b.low) - y(b.high)}" fill="${c("--u-info-bg")}"/>`;
  ticks.forEach((v) => {
    s += `<line x1="${L}" x2="${W - R}" y1="${y(v)}" y2="${y(v)}" stroke="${c("--line")}" stroke-dasharray="${v === b.severe ? "4 4" : "0"}"/>`;
    s += `<text x="${L - 6}" y="${y(v) + 4}" text-anchor="end" font-size="12" fill="${c("--ink-soft")}">${fmtValue(v)}</text>`;
  });
  for (let d = 0; d <= 14; d += 7) {
    const t = now - (14 - d) * 864e5;
    s += `<text x="${x(t)}" y="${H - 6}" text-anchor="${d === 0 ? "start" : d === 14 ? "end" : "middle"}" font-size="12" fill="${c("--ink-soft")}">${d === 14 ? "Today" : new Date(t).toLocaleDateString([], { day: "numeric", month: "short" })}</text>`;
  }
  const col = { info: "--u-info", monitor: "--u-monitor", action: "--u-action", urgent: "--u-urgent", emergency: "--u-urgent" };
  const pts = rs.map((r) => [x(Date.parse(r.measured_at)), y(r.decision.value_mg_dl), r]);
  if (pts.length > 1) s += `<polyline fill="none" stroke="${c("--ink-soft")}" stroke-opacity=".35" stroke-width="1.5" points="${pts.map((p) => p[0].toFixed(1) + "," + p[1].toFixed(1)).join(" ")}"/>`;
  pts.forEach(([px, py, r]) => {
    const v = r.decision.value_mg_dl;
    const cc = v < b.low ? c("--u-urgent") : v > b.high ? c("--u-action") : c(col[r.decision.urgency] || "--u-info");
    s += `<circle cx="${px.toFixed(1)}" cy="${py.toFixed(1)}" r="5" fill="${cc}" stroke="${c("--surface")}" stroke-width="2"><title>${fmtValue(v)} ${state.profile.unit}, ${new Date(r.measured_at).toLocaleString()}</title></circle>`;
  });
  return s + "</svg>";
}

function bindHistory() {}

function downloadCSV() {
  const head = ["measured_at", "value", "unit", "meter_flag", "value_mg_dl", "meal", "symptoms", "urgency", "action_code", "message", "rule_set_version"];
  const cell = (v) => {
    let s = String(v ?? "");
    if (/^[=+\-@]/.test(s)) s = "'" + s;
    return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  };
  const rows = state.readings.map((r) => [r.measured_at, r.meter_flag ? "" : r.value, r.unit, r.meter_flag, r.decision?.value_mg_dl,
    r.meal, [...(r.symptoms || []), ...(r.extra_symptoms || [])].join(";"), r.decision?.urgency, r.decision?.action_code,
    META.messages[r.decision?.approved_message_id]?.title, r.decision?.rule_set_version].map(cell).join(","));
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([[head.join(","), ...rows].join("\n")], { type: "text/csv" }));
  a.download = `glucose-readings-${new Date().toISOString().slice(0, 10)}.csv`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

/* ---- plan ---- */

function planView() {
  const p = state.profile, pl = state.plan || {};
  const d = META.default_plan;
  const u = p.unit;
  const show = (mg) => (mg === undefined || mg === "" ? "" : fmtValue(+mg, u));
  const sel = (id, opts, v) => `<select id="${id}">${opts.map(([k, l]) => `<option value="${k}" ${k === v ? "selected" : ""}>${esc(l)}</option>`).join("")}</select>`;
  const outOfScope = !["adult", "older_adult"].includes(p.age_group) || p.pregnant || p.diabetes_type === "gestational" || p.special_circumstances;
  return `
    <h2>About you</h2>
    <label class="field"><span>Age group</span>${sel("age", [["adult", "Adult (18–64)"], ["older_adult", "Older adult (65+)"], ["adolescent", "Teenager"], ["child", "Child"]], p.age_group)}</label>
    <label class="field"><span>Diabetes type</span>${sel("dtype", [["type1", "Type 1"], ["type2", "Type 2"], ["gestational", "Gestational"], ["other", "Other / not sure"]], p.diabetes_type)}</label>
    <label class="check"><input type="checkbox" id="preg" ${p.pregnant ? "checked" : ""}><span>I'm pregnant</span></label>
    <label class="check"><input type="checkbox" id="special" ${p.special_circumstances ? "checked" : ""}><span>My doctor has given me a special plan (for example dialysis, or I don't feel my lows)</span></label>
    ${outOfScope ? `<p class="notice"><strong>The app won't give general steps for you.</strong> For children, pregnancy and special plans, it saves your readings and tells you to follow the plan from your care team. Emergency guidance still works.</p>` : ""}
    <label class="field"><span>Show readings in</span>${sel("unit", [["mg/dL", "mg/dL (most meters in India)"], ["mmol/L", "mmol/L"]], u)}</label>

    <h2>Targets from your doctor</h2>
    <p class="muted">Without these, the app uses general low thresholds (${fmtValue(d.low_mg_dl, u)} and ${fmtValue(d.severe_low_mg_dl, u)}) and tells you to contact your care team for high readings.</p>
    <label class="field"><span>Low below</span><input id="low" type="number" step="any" inputmode="decimal" value="${show(pl.low_mg_dl)}" placeholder="${fmtValue(70, u)}"></label>
    <label class="field"><span>Very low below</span><input id="slow" type="number" step="any" inputmode="decimal" value="${show(pl.severe_low_mg_dl)}" placeholder="${fmtValue(54, u)}"></label>
    <label class="field"><span>High above</span><input id="high" type="number" step="any" inputmode="decimal" value="${show(pl.high_mg_dl)}" placeholder="${fmtValue(180, u)}"></label>
    <label class="field"><span>Very high above</span><input id="vhigh" type="number" step="any" inputmode="decimal" value="${show(pl.very_high_mg_dl)}" placeholder="${fmtValue(300, u)}"></label>
    <label class="field"><span>Doctor's name</span><input id="doc" type="text" maxlength="80" value="${esc(pl.doctor || "")}" autocomplete="off"></label>
    <label class="check"><input type="checkbox" id="fromdoc" ${pl.from_doctor ? "checked" : ""}><span>My doctor gave me these targets</span></label>
    <p class="error" id="perr" role="alert"></p>
    <div class="row">
      <button class="btn btn-secondary" id="saveplan">Save</button>
      ${state.plan ? `<button class="btn btn-quiet" id="clearplan">Remove targets</button>` : ""}
    </div>

    <h2>Reminders</h2>
    <p class="muted">When a recheck is due, the app reminds you while it's open. Phones may stop this when the app is closed, so you can also add each recheck to your calendar.</p>
    <button class="btn btn-quiet" id="notif">${"Notification" in window && Notification.permission === "granted" ? "Notifications are on" : "Turn on notifications"}</button>

    <h2>Your data</h2>
    <p class="muted">${state.readings.length} readings stored only on this phone. Clearing your browser data deletes them.</p>
    <div class="row">
      <button class="btn btn-quiet" id="csv2">Download CSV</button>
      <button class="btn btn-danger" id="wipe">Delete all my data</button>
    </div>

    <h2>About</h2>
    <p class="muted small">Safety rules ${esc(META.rule_set_version)}. Preview only: the rules and messages have not been reviewed by a diabetes doctor yet, and the app never suggests insulin doses or medicine changes. In an emergency call 112.
    <a href="https://github.com/aditi5/glucoguard">Source code</a>.</p>`;
}

function bindPlan() {
  const p = state.profile;
  const onProfile = () => {
    p.age_group = $("#age").value; p.diabetes_type = $("#dtype").value;
    p.pregnant = $("#preg").checked; p.special_circumstances = $("#special").checked; p.unit = $("#unit").value;
    save(); render(); toast("Saved");
  };
  ["#age", "#dtype", "#preg", "#special", "#unit"].forEach((s) => $(s).addEventListener("change", onProfile));

  $("#saveplan").addEventListener("click", () => {
    const u = p.unit, toMg = (s) => { const v = parseFloat(s); return u === "mmol/L" ? Math.round(v * MGDL_PER_MMOL * 10) / 10 : v; };
    const vals = ["#low", "#slow", "#high", "#vhigh"].map((s) => $(s).value.trim());
    const err = $("#perr");
    if (vals.some((v) => v === "" || isNaN(parseFloat(v)))) { err.textContent = "Fill in all four targets."; return; }
    const fromDoc = $("#fromdoc").checked, doc = $("#doc").value.trim();
    if (fromDoc && !doc) { err.textContent = "Add your doctor's name, or untick the box."; return; }
    const plan = { version: "self-" + Date.now(), low_mg_dl: toMg(vals[0]), severe_low_mg_dl: toMg(vals[1]), high_mg_dl: toMg(vals[2]),
      very_high_mg_dl: toMg(vals[3]), from_doctor: fromDoc, doctor: doc };
    const prev = state.plan; state.plan = plan;
    const check = window.glucoguard.validatePlan(JSON.stringify(enginePlan()));
    if (check.error) {
      state.plan = prev;
      err.textContent = "These targets don't fit together: very low must be below low, low below high, and high no more than very high.";
      return;
    }
    save(); render(); toast("Targets saved");
  });
  $("#clearplan")?.addEventListener("click", () => { state.plan = null; save(); render(); toast("Targets removed"); });
  $("#notif").addEventListener("click", async () => {
    if (!("Notification" in window)) { toast("This browser doesn't support notifications."); return; }
    const res = await Notification.requestPermission();
    toast(res === "granted" ? "Notifications are on" : "Notifications are blocked in your browser settings.");
    render();
  });
  $("#csv2").addEventListener("click", downloadCSV);
  $("#wipe").addEventListener("click", () => {
    if (!confirm("Delete every reading and setting on this phone? This can't be undone.")) return;
    try { localStorage.removeItem(STORE_KEY); } catch (_) {}
    state = defaults(); tab = "today"; render(); toast("All data deleted");
  });
}

/* ---------------- boot ---------------- */

$("#app").addEventListener("click", onAppClick);
document.querySelectorAll(".tabs button").forEach((b) =>
  b.addEventListener("click", () => { tab = b.dataset.tab; render(); window.scrollTo(0, 0); $("#app").focus({ preventScroll: true }); }));

async function boot() {
  try {
    const go = new Go();
    const ready = new Promise((res) => window.addEventListener("glucoguard-ready", res, { once: true }));
    const resp = fetch("engine.wasm");
    const result = WebAssembly.instantiateStreaming
      ? await WebAssembly.instantiateStreaming(resp, go.importObject).catch(async () =>
          WebAssembly.instantiate(await (await fetch("engine.wasm")).arrayBuffer(), go.importObject))
      : await WebAssembly.instantiate(await (await resp).arrayBuffer(), go.importObject);
    go.run(result.instance);
    await ready;
    META = JSON.parse(window.glucoguard.meta().ok);
  } catch (e) {
    console.error(e);
    $("#app").innerHTML = `<section class="empty"><h1>The safety checks didn't load</h1>
      <p>Reload the page. If it keeps happening, use a recent version of Chrome or Safari.</p>
      <p><strong>If you feel unwell, follow your care plan. In an emergency call <a href="tel:112">112</a>.</strong></p></section>`;
    return;
  }
  render();
  scheduleReminder();
  document.addEventListener("visibilitychange", () => { if (!document.hidden && tab === "today") render(); });
  if ("serviceWorker" in navigator) navigator.serviceWorker.register("sw.js").catch(() => {});
}
boot();
