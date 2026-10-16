// Learn — Audio Courses player. Plain JS, no build step. Every URL is relative so the page works under any prefix.
(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const app = $("app"), audio = $("audio");
  const SPEEDS = [1, 1.15, 1.3, 1.5, 1.75, 2, 0.85];
  const store = {
    get(k, d) { try { const v = localStorage.getItem("learn." + k); return v == null ? d : JSON.parse(v); } catch { return d; } },
    set(k, v) { try { localStorage.setItem("learn." + k, JSON.stringify(v)); } catch {} },
  };

  // ---------- helpers ----------
  async function api(path, opts = {}) {
    const init = { method: opts.method || "GET", headers: {} };
    if (opts.body instanceof FormData) init.body = opts.body;
    else if (opts.body !== undefined) { init.body = JSON.stringify(opts.body); init.headers["Content-Type"] = "application/json"; }
    const r = await fetch("api/" + path, init);
    let data = null;
    try { data = await r.json(); } catch {}
    if (!r.ok) throw new Error((data && data.error) || r.status + " " + r.statusText);
    return data;
  }
  function h(tag, attrs = {}, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v == null || v === false) continue;
      if (k === "class") el.className = v;
      else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
      else if (k === "html") el.innerHTML = v;
      else el.setAttribute(k, v === true ? "" : v);
    }
    for (const kid of kids.flat()) if (kid != null && kid !== false) el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
    return el;
  }
  const fmt = (t) => { t = Math.max(0, t || 0); const m = Math.floor(t / 60), s = Math.floor(t % 60); return m + ":" + String(s).padStart(2, "0"); };
  function hours(sec) {
    sec = Math.round(sec || 0);
    if (sec < 60) return sec + "s";
    const hh = Math.floor(sec / 3600), mm = Math.round((sec % 3600) / 60);
    return hh ? hh + "h " + mm + "m" : mm + " min";
  }
  function toast(msg, action, fn, ms = 7000) {
    document.querySelectorAll(".toast").forEach((t) => t.remove());
    const t = h("div", { class: "toast", role: "status" }, h("span", {}, msg), action ? h("button", { onclick: () => { t.remove(); fn(); } }, action) : null);
    document.body.append(t);
    setTimeout(() => t.remove(), ms);
  }

  let me = null, voices = [];
  let timers = [];
  function clearTimers() { timers.forEach(clearInterval); timers = []; }

  // ---------- routing ----------
  async function route() {
    clearTimers();
    const m = location.hash.match(/^#\/c\/([0-9a-f]+)/);
    if (m) return courseView(m[1]);
    player.detach();
    return topicsView();
  }
  addEventListener("hashchange", route);

  // ---------- topics ----------
  function statusPill(c) {
    if (c.status === "active") return c.paused ? h("span", { class: "pill" }, "paused") : h("span", { class: "pill live" }, "playing");
    if (c.status === "error") return h("span", { class: "pill bad" }, "needs attention");
    if (c.status === "awaiting") return h("span", { class: "pill busy" }, "answer a few questions");
    return h("span", { class: "pill busy" }, h("span", { class: "spinner" }), c.status === "planning" ? "planning" : "getting ready");
  }

  async function topicsView() {
    document.title = (me && me.title) || "Learn";
    const list = h("div", { class: "topics" }, h("p", { class: "muted" }, "Loading your topics…"));
    const formBox = h("div");
    app.replaceChildren(
      h("header", { class: "spread" },
        h("div", {}, h("div", { class: "eyebrow" }, "Audio courses"), h("h1", {}, "What are you learning?")),
        h("button", { class: "primary", onclick: () => { formBox.replaceChildren(newTopicForm()); formBox.scrollIntoView({ behavior: "smooth" }); } }, "New topic")),
      formBox, list);
    try {
      const cs = await api("courses");
      list.replaceChildren();
      if (!cs.length) {
        formBox.replaceChildren(newTopicForm());
        list.append(h("p", { class: "muted" }, "No topics yet. Start one above: anything from how TLS works to how your own codebase fits together."));
      }
      for (const c of cs) {
        list.append(h("button", { class: "topic", onclick: () => (location.hash = "#/c/" + c.id) },
          h("div", { class: "spread" }, h("b", {}, c.title || c.topic), statusPill(c)),
          h("span", { class: "muted small" },
            [c.can_steer ? null : "shared with you", hours(c.ready_s) + " ready", c.played + " of " + c.segments + " segments played", c.provider + (c.model ? " · " + c.model : "")].filter(Boolean).join(" · "))));
      }
    } catch (e) { list.replaceChildren(h("p", { class: "muted" }, "Couldn't load topics: " + e.message)); }
  }

  function voicePicker(selected, name) {
    const box = h("div", { class: "voices" });
    if (!voices.length) box.append(h("p", { class: "muted small" }, "The voice engine isn't answering yet. The default voice will be used."));
    voices.forEach((v, i) => {
      const id = name + "-" + v.id;
      const prev = h("button", { type: "button", class: "quiet small", onclick: (ev) => previewVoice(v.id, ev.currentTarget) }, "Preview");
      box.append(h("label", { class: "voice", for: id },
        h("input", { type: "radio", name, id, value: v.id, checked: (selected ? v.id === selected : i === 0) }),
        h("span", {}, v.label, " ", h("span", { class: "muted small" }, v.engine)), prev));
    });
    return box;
  }
  let previewAudio = null;
  function previewVoice(id, btn) {
    if (previewAudio) { previewAudio.pause(); previewAudio = null; }
    btn.textContent = "Loading…";
    previewAudio = new Audio("api/voices/" + encodeURIComponent(id) + "/preview");
    previewAudio.oncanplay = () => { btn.textContent = "Preview"; };
    previewAudio.onerror = () => { btn.textContent = "Unavailable"; };
    previewAudio.play().catch(() => { btn.textContent = "Preview"; });
  }

  function providerFields(provider, model) {
    const sel = h("select", { id: "f-provider" }, ...(me ? me.providers : ["claude", "codex"]).map((p) =>
      h("option", { value: p, selected: p === (provider || "claude") }, p === "claude" ? "Claude" : p === "codex" ? "ChatGPT (Codex)" : p === "gemini" ? "Gemini" : p)));
    return h("div", { class: "row", style: "align-items:end" },
      h("label", { class: "field", style: "flex:1;min-width:160px" }, h("span", {}, "Teacher model"), sel),
      h("label", { class: "field", style: "flex:1;min-width:160px" }, h("span", {}, "Model (optional)"),
        h("input", { type: "text", id: "f-model", value: model || "", placeholder: "the CLI's default, e.g. opus", autocomplete: "off" })));
  }

  function newTopicForm() {
    const err = h("p", { class: "small", style: "color:var(--bad)" });
    const form = h("form", { class: "panel" },
      h("h2", {}, "New topic"),
      h("label", { class: "field" }, h("span", {}, "What do you want to learn?"),
        h("input", { type: "text", id: "f-topic", required: true, placeholder: "e.g. Cybersecurity: how real attacks work and how to defend against them", autocomplete: "off" })),
      h("label", { class: "field" }, h("span", {}, "Why, or what for? (optional)"),
        h("input", { type: "text", id: "f-goal", placeholder: "e.g. reason about our product's threat model; pass an interview", autocomplete: "off" })),
      h("label", { class: "field" }, h("span", {}, "What do you already know? (optional)"),
        h("textarea", { id: "f-bg", placeholder: "Be honest; it sets where we start. A short check comes next either way." })),
      h("div", { class: "field" }, h("span", { style: "font-weight:500" }, "Voice"), voicePicker("", "f-voice")),
      providerFields("claude", ""),
      h("label", { class: "row small" }, h("input", { type: "checkbox", id: "f-skip" }), "Skip the level check and start right away"),
      err,
      h("div", { class: "row" }, h("button", { class: "primary", type: "submit" }, "Start learning"), h("button", { type: "button", class: "quiet", onclick: () => form.remove() }, "Cancel")));
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const btn = form.querySelector("button[type=submit]");
      btn.disabled = true; err.textContent = "";
      const voice = form.querySelector("input[name=f-voice]:checked");
      try {
        const c = await api("courses", { method: "POST", body: {
          topic: $("f-topic").value, goal: $("f-goal").value, background: $("f-bg").value,
          voice_id: voice ? voice.value : "", provider: $("f-provider").value, model: $("f-model").value,
          skip_calibration: $("f-skip").checked,
        } });
        location.hash = "#/c/" + c.id;
      } catch (e) { err.textContent = e.message; btn.disabled = false; }
    });
    setTimeout(() => $("f-topic") && $("f-topic").focus(), 50);
    return form;
  }

  // ---------- course ----------
  let view = null; // {id, data, els}

  async function courseView(id) {
    if (!view || view.id !== id) { view = { id, data: null }; player.detach(); }
    app.replaceChildren(h("p", { class: "muted" }, "Loading…"));
    await refreshCourse(true);
    timers.push(setInterval(() => refreshCourse(false), 8000));
  }

  async function refreshCourse(first) {
    if (!view) return;
    let d;
    try { d = await api("courses/" + view.id); } catch (e) {
      if (first) app.replaceChildren(h("p", {}, "Couldn't open this topic: " + e.message), h("a", { href: "#/" }, "Back to topics"));
      return;
    }
    const prevStatus = view.data && view.data.course.status;
    view.data = d;
    const c = d.course;
    document.title = (c.title || c.topic) + " · Learn";
    if (first || prevStatus !== c.status) renderCourse();
    else updateLive();
  }

  function renderCourse() {
    const d = view.data, c = d.course;
    const els = view.els = {};
    const header = h("header", { class: "stack" },
      h("a", { href: "#/", class: "small" }, "← Topics"),
      h("div", { class: "spread" }, h("h1", {}, c.title || c.topic), statusPill(c)),
      els.meta = h("div", { class: "muted small" }));
    const body = [header];

    if (c.status === "calibrating" || c.status === "planning") {
      body.push(h("div", { class: "panel" }, h("p", {}, h("span", { class: "spinner" }), " ",
        c.status === "calibrating" ? "Writing a few quick questions to find your level…" : "Planning your curriculum around your answers…"),
        h("p", { class: "muted small" }, "This takes a minute or two. You can leave this page; it carries on.")));
    } else if (c.status === "awaiting" && d.can_steer) {
      body.push(calibrationForm(c));
    } else if (c.status === "error") {
      body.push(h("div", { class: "panel" }, h("p", {}, "Something went wrong setting this up."), h("p", { class: "muted small" }, c.error),
        d.can_steer ? h("button", { class: "primary", onclick: async () => { await api("courses/" + c.id + "/retry", { method: "POST" }); refreshCourse(true); } }, "Try again") : null));
    } else if (c.status === "active") {
      body.push(playerSection(d));
      if (d.can_steer) body.push(askSection());
      body.push(els.qa = h("section", { class: "panel" }, h("h2", {}, "Your questions"), h("p", { class: "muted small" }, "Nothing asked yet.")));
      body.push(outlineSection(d));
      if (d.can_steer) body.push(settingsSection(d));
    }
    app.replaceChildren(...body);
    updateLive();
    if (c.status === "active") {
      player.attach(view.id, d);
      loadInteractions();
    }
  }

  function updateLive() {
    const d = view.data, c = d.course, s = d.stats, els = view.els || {};
    if (els.meta) {
      const bits = [c.provider + (c.model ? " · " + c.model : ""), voiceLabel(c.voice_id)];
      if (c.status === "active") {
        bits.push(hours(s.ahead_s) + " ready ahead");
        const busy = Object.keys(s.jobs || {}).length;
        if (busy) bits.push("generating");
        if (c.paused) bits.push("generation paused");
      }
      els.meta.textContent = bits.filter(Boolean).join(" · ");
      if (c.error && c.status === "active") els.meta.append(h("div", { style: "color:var(--warn)" }, c.error));
    }
    if (els.outline) els.outline.replaceChildren(...outlineItems(d));
  }

  function voiceLabel(id) { const v = voices.find((x) => x.id === id); return v ? v.label : id; }

  function calibrationForm(c) {
    const qs = c.calibration.questions || [];
    const areas = qs.map((q, i) => h("textarea", { id: "cal-" + i, placeholder: "Your answer (or leave blank if you don't know)" }));
    const form = h("form", { class: "panel" },
      h("h2", {}, "A quick level check"),
      h("p", { class: "muted small" }, "Answer in a sentence or two, typed or spoken. \"No idea\" is a useful answer too."),
      ...qs.map((q, i) => h("div", { class: "field stack" }, h("span", { style: "font-weight:500" }, (i + 1) + ". " + q.q), h("div", { class: "row", style: "flex-wrap:nowrap;align-items:start" }, areas[i], micButton((t) => { areas[i].value = (areas[i].value ? areas[i].value + " " : "") + t; })))),
      h("div", { class: "row" }, h("button", { class: "primary", type: "submit" }, "Plan my course"),
        h("button", { type: "button", class: "quiet", onclick: () => submit(true) }, "Skip")));
    async function submit(skip) {
      form.querySelectorAll("button").forEach((b) => (b.disabled = true));
      try { await api("courses/" + c.id + "/calibration", { method: "POST", body: { answers: areas.map((a) => a.value), skip } }); refreshCourse(true); }
      catch (e) { toast(e.message); form.querySelectorAll("button").forEach((b) => (b.disabled = false)); }
    }
    form.addEventListener("submit", (ev) => { ev.preventDefault(); submit(false); });
    return form;
  }

  // ---------- voice input ----------
  function micButton(onText) {
    let rec = null, chunks = [];
    const btn = h("button", { type: "button", class: "mic", "aria-label": "Speak" }, "Speak");
    if (!navigator.mediaDevices || !window.MediaRecorder) { btn.disabled = true; btn.title = "Voice input needs HTTPS and a browser with a microphone."; return btn; }
    btn.addEventListener("click", async () => {
      if (rec && rec.state === "recording") { rec.stop(); return; }
      try {
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        chunks = [];
        rec = new MediaRecorder(stream);
        rec.ondataavailable = (e) => e.data.size && chunks.push(e.data);
        rec.onstop = async () => {
          stream.getTracks().forEach((t) => t.stop());
          btn.classList.remove("rec"); btn.textContent = "…"; btn.disabled = true;
          try {
            const fd = new FormData();
            fd.append("audio", new Blob(chunks, { type: rec.mimeType || "audio/webm" }), "speech");
            const r = await api("courses/" + view.id + "/transcribe", { method: "POST", body: fd });
            if (r.text) onText(r.text); else toast("I didn't catch anything. Try again a little closer to the mic.");
          } catch (e) { toast("Couldn't transcribe: " + e.message); }
          btn.textContent = "Speak"; btn.disabled = false;
        };
        rec.start();
        btn.classList.add("rec"); btn.textContent = "Stop";
      } catch (e) { toast("Microphone unavailable: " + e.message); }
    });
    return btn;
  }

  // ---------- ask / check-in ----------
  function askSection() {
    const ta = h("textarea", { id: "ask-text", placeholder: "Ask anything, or steer: \"go deeper on this\", \"I already know TLS\", \"give me a real example\"…" });
    const send = h("button", { class: "primary", type: "submit" }, "Send");
    const form = h("form", { class: "panel ask" }, h("h2", {}, "Ask or steer"), ta,
      h("div", { class: "row" }, send, micButton((t) => { ta.value = (ta.value ? ta.value + " " : "") + t; ta.focus(); }),
        h("span", { class: "muted small" }, "The answer plays next, and what follows adapts.")));
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const text = ta.value.trim();
      if (!text) return;
      send.disabled = true;
      try {
        await api("courses/" + view.id + "/ask", { method: "POST", body: { text, kind: "ask", segment_id: player.currentId(), offset_ms: Math.round(audio.currentTime * 1000) } });
        ta.value = "";
        toast("Got it. Your answer is being written and will play next.");
        loadInteractions();
      } catch (e) { toast(e.message); }
      send.disabled = false;
    });
    view.els.checkin = h("div");
    return h("div", { class: "stack" }, view.els.checkin, form);
  }

  function showCheckin(seg) {
    const box = view && view.els && view.els.checkin;
    if (!box || !view.data.can_steer || !seg.checkin || box.dataset.seg === seg.id) return;
    box.dataset.seg = seg.id;
    const ta = h("textarea", { placeholder: "Answer out loud with Speak, or type it" });
    const send = h("button", { class: "primary", type: "submit" }, "Answer");
    const form = h("form", { class: "panel checkin" }, h("div", { class: "eyebrow" }, "Check-in"), h("p", { style: "margin:0;font-family:var(--font-read);font-size:1.1rem" }, seg.checkin), ta,
      h("div", { class: "row" }, send, micButton((t) => { ta.value = (ta.value ? ta.value + " " : "") + t; }), h("button", { type: "button", class: "quiet", onclick: () => box.replaceChildren() }, "Skip")));
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      if (!ta.value.trim()) return;
      send.disabled = true;
      try {
        await api("courses/" + view.id + "/ask", { method: "POST", body: { text: ta.value, kind: "checkin", segment_id: seg.id } });
        box.replaceChildren(); toast("Thanks. Feedback on your answer plays next."); loadInteractions();
      } catch (e) { toast(e.message); send.disabled = false; }
    });
    box.replaceChildren(form);
  }

  let pendingAsks = new Set();
  async function loadInteractions() {
    if (!view || !view.els || !view.els.qa) return;
    let is;
    try { is = await api("courses/" + view.id + "/interactions"); } catch { return; }
    const qa = view.els.qa;
    qa.replaceChildren(h("h2", {}, "Your questions"));
    if (!is.length) qa.append(h("p", { class: "muted small" }, "Nothing asked yet."));
    for (const i of is.slice(0, 12)) {
      const st = i.status === "pending" ? h("span", { class: "pill busy" }, h("span", { class: "spinner" }), "answering")
        : i.status === "failed" ? h("span", { class: "pill bad" }, "failed") : h("span", { class: "pill live" }, i.verdict ? i.verdict : "answered");
      const play = i.answer_segment_id ? h("button", { class: "quiet small", onclick: () => player.jumpTo(i.answer_segment_id) }, "Play answer") : null;
      qa.append(h("div", { class: "qa" }, h("div", { class: "spread" }, h("span", { class: "muted small" }, i.kind === "checkin" ? "Check-in answer" : "You asked"), st),
        h("div", {}, i.text), play));
      if (i.status === "pending") pendingAsks.add(i.id);
      else if (pendingAsks.has(i.id)) {
        pendingAsks.delete(i.id);
        if (i.status === "answered") player.refreshQueue();
      }
    }
    if (pendingAsks.size && !view.askTimer) {
      view.askTimer = setInterval(() => { if (!pendingAsks.size) { clearInterval(view.askTimer); view.askTimer = null; } loadInteractions(); }, 5000);
      timers.push(view.askTimer);
    }
  }

  // ---------- outline ----------
  function outlineSection(d) {
    view.els.outline = h("div", { class: "outline" });
    return h("section", { class: "panel" }, h("h2", {}, "Curriculum"), h("p", { class: "muted small" }, "Bars show how well you know each concept, from your answers and questions. It grows as you go."), view.els.outline);
  }
  function outlineItems(d) {
    const cur = player.currentSeg();
    const mods = d.nodes.filter((n) => n.kind === "module");
    return mods.map((m) => h("div", { class: "module" },
      h("div", { class: "eyebrow" }, m.title),
      ...d.nodes.filter((n) => n.parent_id === m.id).map((n) => h("div", { class: "concept" + (n.status === "done" ? " done" : "") + (cur && cur.node_id === n.id ? " now" : "") },
        h("span", { class: "ctitle" }, n.title), h("div", { class: "meter", title: "mastery " + Math.round(n.mastery * 100) + "%" }, h("i", { style: "width:" + Math.round(n.mastery * 100) + "%" }))))));
  }

  // ---------- settings ----------
  function settingsSection(d) {
    const c = d.course;
    const memberInput = h("input", { type: "text", placeholder: "their Tailscale login, e.g. friend@example.com", autocomplete: "off" });
    const members = h("div", { class: "small muted" }, (d.members || []).length ? "Listening: " + d.members.join(", ") : "Not shared yet.");
    const box = h("details", { class: "panel" }, h("summary", {}, h("b", {}, "Settings")),
      h("div", { class: "field" }, h("span", { style: "font-weight:500" }, "Voice (applies to new segments)"), voicePicker(c.voice_id, "s-voice")),
      providerFields(c.provider, c.model),
      h("div", { class: "row" },
        h("button", { onclick: async () => {
          const v = box.querySelector("input[name=s-voice]:checked");
          await api("courses/" + c.id, { method: "PATCH", body: { voice_id: v ? v.value : c.voice_id, provider: $("f-provider").value, model: $("f-model").value } });
          toast("Saved."); refreshCourse(true);
        } }, "Save"),
        h("button", { onclick: async () => { await api("courses/" + c.id, { method: "PATCH", body: { paused: !c.paused } }); refreshCourse(true); } }, c.paused ? "Resume generating" : "Pause generating")),
      h("div", { class: "field" }, h("span", { style: "font-weight:500" }, "Share (listen only)"), members,
        h("div", { class: "row", style: "flex-wrap:nowrap" }, memberInput, h("button", { onclick: async () => {
          if (!memberInput.value.trim()) return;
          try { const r = await api("courses/" + c.id + "/members", { method: "POST", body: { user: memberInput.value, add: true } }); members.textContent = "Listening: " + r.members.join(", "); memberInput.value = ""; }
          catch (e) { toast(e.message); }
        } }, "Add"))),
      h("div", { class: "row" }, h("button", { class: "danger", onclick: (ev) => {
        const b = ev.currentTarget;
        if (b.dataset.armed) { api("courses/" + c.id, { method: "DELETE" }).then(() => (location.hash = "#/")); return; }
        b.dataset.armed = "1"; b.textContent = "Tap again to delete this topic and its audio";
      } }, "Delete topic")));
    return box;
  }

  // ---------- player ----------
  function playerSection(d) {
    const mode = store.get("mode", "both");
    view.els.modes = h("div", { class: "modes", role: "group", "aria-label": "How to follow" },
      ...[["both", "Read + listen"], ["listen", "Listen"], ["read", "Read"]].map(([k, l]) => h("button", { "data-mode": k, "aria-pressed": String(mode === k), onclick: () => player.setMode(k) }, l)));
    view.els.reader = h("article", { class: "reader follow" }, h("p", { class: "muted", style: "font-family:var(--font-ui);font-size:0.95rem" }, "Getting the first segment ready…"));
    return h("section", { class: "stack" }, h("div", { class: "spread" }, h("h2", {}, "Now"), view.els.modes), view.els.reader);
  }

  const player = (() => {
    let courseId = null, data = null;
    let cur = null;              // current segment
    let upcoming = [];           // ready segments after cur
    let words = [], spans = [], lit = -1;
    let mode = store.get("mode", "both");
    let userScrolled = false, waiting = false, resumeAt = 0, lastSave = 0, attached = false;
    let speedIdx = Math.max(0, SPEEDS.indexOf(store.get("speed", 1)));

    function detach() {
      attached = false; courseId = null; cur = null; upcoming = []; words = []; spans = [];
      audio.pause(); audio.removeAttribute("src"); $("bar").hidden = true; $("back").hidden = true;
    }

    async function attach(id, d) {
      const same = attached && courseId === id;
      courseId = id; data = d; attached = true;
      $("bar").hidden = false;
      if (same && cur) { renderReader(); return; }
      cur = null; upcoming = [];
      const pb = d.playback;
      if (pb && pb.segment_id) {
        try { cur = await api("courses/" + id + "/segments/" + pb.segment_id); resumeAt = (pb.offset_ms || 0) / 1000; } catch { cur = null; }
        if (cur && cur.status !== "ready") cur = null;
      }
      await refreshQueue();
      if (!cur && upcoming.length) { cur = upcoming.shift(); resumeAt = 0; }
      load(false);
    }

    async function refreshQueue() {
      if (!courseId) return;
      try {
        const q = await api("courses/" + courseId + "/queue?n=4" + (cur ? "&after=" + cur.id : ""));
        upcoming = q.segments; waiting = !upcoming.length && q.generating;
      } catch {}
      renderBar();
      if (!cur && upcoming.length) { cur = upcoming.shift(); resumeAt = 0; load(false); }
      if (mode === "read") renderReader();
    }

    function load(autoplay) {
      if (!cur) { renderReader(); renderBar(); return; }
      words = Array.isArray(cur.words) ? cur.words : [];
      lit = -1;
      audio.src = "api/courses/" + courseId + "/audio/" + cur.id;
      audio.playbackRate = SPEEDS[speedIdx];
      const at = resumeAt; resumeAt = 0;
      audio.onloadedmetadata = () => { if (at > 0 && at < audio.duration - 1) audio.currentTime = at; audio.playbackRate = SPEEDS[speedIdx]; };
      renderReader(); renderBar(); setMediaSession();
      if (view && view.els && view.els.outline) view.els.outline.replaceChildren(...outlineItems(view.data));
      if (autoplay) audio.play().catch(() => {});
    }

    async function advance(autoplay = true) {
      const finished = cur;
      await save(finished && finished.id, true);
      await refreshQueue();
      if (upcoming.length) { cur = upcoming.shift(); load(autoplay); refreshQueueSoon(); return; }
      // Nothing ready yet: wait for the next segment and carry on by itself.
      waiting = true; renderBar();
      const poll = setInterval(async () => {
        if (!attached || courseId == null) return clearInterval(poll);
        await refreshQueue();
        if (upcoming.length) { clearInterval(poll); cur = upcoming.shift(); waiting = false; load(autoplay); }
      }, 5000);
      timers.push(poll);
    }
    let soon = null;
    function refreshQueueSoon() { clearTimeout(soon); soon = setTimeout(refreshQueue, 1500); }

    async function jumpTo(segId) {
      try {
        const g = await api("courses/" + courseId + "/segments/" + segId);
        if (g.status !== "ready") { toast("That answer is still being voiced. It'll play next automatically."); return; }
        await save(cur && cur.id, false);
        cur = g; resumeAt = 0; load(true); refreshQueueSoon();
        scrollTo({ top: 0, behavior: "smooth" });
      } catch (e) { toast(e.message); }
    }

    async function save(finishedId, finished) {
      if (!cur || !courseId) return;
      lastSave = Date.now();
      try {
        await api("courses/" + courseId + "/playback", { method: "PUT", body: {
          segment_id: cur.id, offset_ms: finished ? Math.round((audio.duration || 0) * 1000) : Math.round(audio.currentTime * 1000),
          rate: SPEEDS[speedIdx], finished: finished ? finishedId : "" } });
      } catch {}
    }

    function setMode(m) {
      mode = m; store.set("mode", m);
      if (view && view.els && view.els.modes) view.els.modes.querySelectorAll("button").forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.mode === m)));
      if (m === "read") audio.pause();
      renderReader();
    }

    function renderReader() {
      const r = view && view.els && view.els.reader;
      if (!r) return;
      r.classList.toggle("follow", mode === "both");
      r.classList.toggle("hide-text", mode === "listen");
      spans = []; lit = -1;
      if (!cur) {
        r.replaceChildren(h("p", { class: "muted", style: "font-family:var(--font-ui);font-size:0.95rem" },
          waiting ? "Writing and voicing the first segment. The first one takes a couple of minutes." : "Getting the first segment ready…"));
        return;
      }
      const chapters = [cur, ...(mode === "read" ? upcoming : [])];
      const nodes = [];
      for (const g of chapters) {
        const ch = h("div", { class: "chapter" }, h("h3", {}, (g.kind === "answer" ? "Answer · " : g.kind === "feedback" ? "Feedback · " : "") + (g.title || "")));
        if (g === cur && words.length && mode !== "read") {
          let p = -1, para = null;
          words.forEach((w, i) => {
            if (w.p !== p) { para = h("p"); ch.append(para); p = w.p; }
            const s = h("span", { class: "w", "data-i": i }, w.w);
            para.append(s, " "); spans.push(s);
          });
        } else {
          for (const para of (g.script || "").split(/\n\s*\n/)) ch.append(h("p", {}, para));
        }
        nodes.push(ch);
      }
      if (mode === "listen") nodes.unshift(h("div", { class: "listen-card" }, h("b", {}, cur.title || "Listening"), "Text is hidden. Switch to Read + listen to follow along."));
      r.replaceChildren(...nodes);
    }

    function renderBar() {
      $("bar-title").textContent = cur ? cur.title || "Segment" : waiting ? "Preparing the next segment…" : "Nothing ready yet";
      $("bar-sub").textContent = cur ? (data ? (data.course.title || data.course.topic) : "") + (upcoming.length ? " · next: " + (upcoming[0].title || "") : waiting ? " · writing the next part…" : "") : "";
      $("play").disabled = !cur;
    }

    function setMediaSession() {
      if (!("mediaSession" in navigator) || !cur) return;
      try {
        navigator.mediaSession.metadata = new MediaMetadata({ title: cur.title || "Learn", artist: data ? data.course.title || data.course.topic : "", album: "Learn",
          artwork: [{ src: "static/icon-512.png", sizes: "512x512", type: "image/png" }] });
      } catch {}
    }
    if ("mediaSession" in navigator) {
      const ms = navigator.mediaSession;
      const set = (a, f) => { try { ms.setActionHandler(a, f); } catch {} };
      set("play", () => audio.play()); set("pause", () => audio.pause());
      set("seekbackward", () => (audio.currentTime = Math.max(0, audio.currentTime - 15)));
      set("seekforward", () => (audio.currentTime = Math.min(audio.duration || 0, audio.currentTime + 15)));
      set("nexttrack", () => advance(true));
    }

    // Read-along: highlight the word being spoken.
    function wordAt(t) {
      let lo = 0, hi = words.length - 1, ans = -1;
      while (lo <= hi) { const m = (lo + hi) >> 1; if (words[m].s <= t) { ans = m; lo = m + 1; } else hi = m - 1; }
      return ans;
    }
    function tick() {
      if (spans.length && mode === "both") {
        const i = wordAt(audio.currentTime);
        if (i !== lit) {
          if (lit >= 0 && spans[lit]) spans[lit].classList.remove("on");
          const a = Math.max(0, Math.min(lit, i)), b = Math.max(lit, i);
          for (let k = a; k <= b && k < spans.length; k++) spans[k].classList.toggle("done", k < i);
          if (i >= 0 && spans[i]) {
            spans[i].classList.add("on");
            if (!userScrolled && !audio.paused) {
              const rc = spans[i].getBoundingClientRect(), vh = innerHeight;
              if (rc.top < vh * 0.2 || rc.bottom > vh * 0.6) spans[i].scrollIntoView({ block: "center", behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
            }
          }
          lit = i;
        }
      }
      if (cur && cur.checkin && audio.duration && audio.currentTime > audio.duration * 0.85) showCheckin(cur);
      const d = audio.duration || 0;
      $("time").textContent = fmt(audio.currentTime) + " / " + fmt(d);
      if (d && !seeking) $("seek").value = String(Math.round((audio.currentTime / d) * 1000));
      if (!audio.paused && Date.now() - lastSave > 10000) save();
      requestAnimationFrame(tick);
    }
    requestAnimationFrame(tick);

    document.addEventListener("click", (e) => {
      const i = e.target && e.target.dataset && e.target.dataset.i;
      if (i == null || !words[+i]) return;
      audio.currentTime = words[+i].s + 0.01; userScrolled = false; $("back").hidden = true;
      if (audio.paused && mode !== "read") audio.play().catch(() => {});
    });
    const manual = () => { if (!audio.paused && mode === "both") { userScrolled = true; $("back").hidden = false; } };
    addEventListener("wheel", manual, { passive: true });
    addEventListener("touchmove", manual, { passive: true });
    $("back").onclick = () => { userScrolled = false; $("back").hidden = true; if (spans[lit]) spans[lit].scrollIntoView({ block: "center", behavior: "smooth" }); };

    audio.addEventListener("ended", () => advance(true));
    audio.addEventListener("play", () => { $("play").textContent = "Pause"; if (mode === "read") setMode("both"); });
    audio.addEventListener("pause", () => { $("play").textContent = "Play"; save(); });
    audio.addEventListener("error", () => { if (cur) toast("Couldn't play this segment; skipping it.", null, null, 4000); setTimeout(() => advance(true), 1500); });
    document.addEventListener("visibilitychange", () => { if (document.visibilityState === "hidden") save(); });

    $("play").onclick = () => { if (!cur) return; audio.paused ? audio.play().catch((e) => toast(e.message)) : audio.pause(); };
    $("rew").onclick = () => { audio.currentTime = Math.max(0, audio.currentTime - 15); };
    $("fwd").onclick = () => { audio.currentTime = Math.min(audio.duration || 0, audio.currentTime + 15); };
    $("next").onclick = () => advance(!audio.paused);
    $("speed").textContent = SPEEDS[speedIdx] + "×";
    $("speed").onclick = () => { speedIdx = (speedIdx + 1) % SPEEDS.length; audio.playbackRate = SPEEDS[speedIdx]; store.set("speed", SPEEDS[speedIdx]); $("speed").textContent = SPEEDS[speedIdx] + "×"; };
    let seeking = false;
    $("seek").oninput = () => { seeking = true; };
    $("seek").onchange = () => { if (audio.duration) audio.currentTime = (+$("seek").value / 1000) * audio.duration; seeking = false; };

    return { attach, detach, refreshQueue, jumpTo, setMode, currentId: () => (cur ? cur.id : ""), currentSeg: () => cur };
  })();

  // ---------- boot ----------
  (async () => {
    try { me = await api("me"); } catch (e) { app.replaceChildren(h("p", {}, "Can't reach Learn: " + e.message)); return; }
    try { voices = await api("voices"); } catch { voices = []; }
    route();
  })();
})();
