// Learn — Audio Courses player. Plain JS, no build step. Every URL is relative so the page works under any prefix.
(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  // Optional pieces are passed as null/false; never let the DOM turn them into the text "null".
  for (const m of ["replaceChildren", "append", "prepend"]) {
    const orig = Element.prototype[m];
    Element.prototype[m] = function (...kids) { return orig.apply(this, kids.filter((k) => k != null && k !== false)); };
  }
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
  function deviceName() {
    const ua = navigator.userAgent;
    const kind = /iPhone/.test(ua) ? "iPhone" : /iPad/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1) ? "iPad"
      : /Android/.test(ua) ? "Android" : /Macintosh/.test(ua) ? "Mac" : /Windows/.test(ua) ? "Windows PC" : "another device";
    return kind;
  }
  const device = (() => {
    let id = store.get("device", "");
    if (!id) { id = Math.random().toString(36).slice(2, 8); store.set("device", id); }
    return deviceName() + " · " + id;
  })();
  const deviceLabel = (d) => (d || "another device").split(" · ")[0];
  let timers = [];
  function clearTimers() { timers.forEach(clearInterval); timers = []; }

  // ---------- routing ----------
  async function route() {
    clearTimers();
    const m = location.hash.match(/^#\/c\/([0-9a-z]+)(?:\/(\w+))?/);
    if (m) { if (m[2]) store.set("tab", m[2]); if (view && m[2]) view.tab = m[2]; return courseView(m[1]); }
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
    const inHub = location.pathname.startsWith("/learn/");
    app.replaceChildren(
      inHub ? h("a", { href: "/", class: "small" }, "← Hub") : null,
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
      (me && me.research_dirs && me.research_dirs.length) ? h("label", { class: "row small" }, h("input", { type: "checkbox", id: "f-research", checked: true }),
        "Let the teacher read my code (" + me.research_dirs.map((d) => d.replace(/^\/Users\/[^/]+/, "~")).join(", ") + ") and GitHub, read-only") : null,
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
          skip_calibration: $("f-skip").checked, research: $("f-research") ? $("f-research").checked : false,
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
      const tabs = [["listen", "Listen"], ["map", "Mind map"]];
      if (d.can_steer) tabs.push(["quiz", "Quiz"], ["questions", "Questions"], ["you", "About you"], ["settings", "Settings"]);
      const panes = {};
      panes.listen = h("div", { class: "stack" }, playerSection(d), d.can_steer ? askSection() : null,
        els.qa = h("section", { class: "panel" }, h("h2", {}, "Latest questions"), h("p", { class: "muted small" }, "Nothing asked yet.")));
      panes.map = mindMapSection(d);
      if (d.can_steer) {
        panes.quiz = els.quiz = h("div", { class: "stack" });
        panes.questions = els.allq = h("div", { class: "stack" });
        panes.you = els.you = h("div", { class: "stack" });
        panes.settings = settingsSection(d);
      }
      const bar = h("nav", { class: "tabs", role: "tablist", "aria-label": "Course" });
      const show = (k) => {
        view.tab = k; store.set("tab", k);
        for (const [key] of tabs) {
          panes[key].hidden = key !== k;
          bar.querySelector('[data-tab="' + key + '"]').setAttribute("aria-selected", String(key === k));
        }
        if (k === "quiz") renderQuizTab();
        if (k === "questions") renderAllQuestions();
        if (k === "you") renderAboutYou();
        if (k === "map") drawMindMap();
      };
      view.show = show;
      for (const [key, label] of tabs) bar.append(h("button", { role: "tab", "data-tab": key, onclick: () => show(key) }, label));
      els.toc = h("nav", { class: "toc", "aria-label": "Contents" });
      els.tocWrap = h("aside", { class: "tocwrap", id: "tocwrap" },
        h("div", { class: "spread" }, h("h2", {}, "Contents"), h("button", { class: "quiet small toc-close", onclick: () => document.body.classList.remove("toc-open") }, "Close")),
        h("p", { class: "muted small", style: "margin:0" }, "Tap to jump there. It grows and changes as you steer; only what's after you changes."), els.toc);
      body.push(h("button", { class: "toc-open-btn", onclick: () => { document.body.classList.add("toc-open"); renderContents(); } }, "Contents"));
      body.push(h("div", { class: "course-grid" }, els.tocWrap, h("div", { class: "course-main stack" }, bar, ...tabs.map(([k]) => panes[k]))));
      view.afterRender = () => show(tabs.some(([k]) => k === view.tab) ? view.tab : (tabs.some(([k]) => k === store.get("tab", "listen")) ? store.get("tab", "listen") : "listen"));
    }
    app.replaceChildren(...body);
    if (view.afterRender) { view.afterRender(); view.afterRender = null; }
    updateLive();
    if (c.status === "active") {
      player.attach(view.id, d);
      loadInteractions();
      renderContents();
      timers.push(setInterval(renderContents, 15000));
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
    if (view.tab === "map") drawMindMap();
    // Nothing playing yet (a new course, or everything heard so far): keep looking for the next segment.
    if (c.status === "active" && !player.currentSeg()) player.refreshQueue();
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
    const btn = h("button", { type: "button", class: "mic icon", "aria-label": "Speak", title: "Speak" }, icon("mic"));
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
          btn.classList.remove("rec"); btn.replaceChildren(h("span", { class: "spinner" })); btn.disabled = true;
          try {
            const fd = new FormData();
            fd.append("audio", new Blob(chunks, { type: rec.mimeType || "audio/webm" }), "speech");
            const r = await api("courses/" + view.id + "/transcribe", { method: "POST", body: fd });
            if (r.text) onText(r.text); else toast("I didn't catch anything. Try again a little closer to the mic.");
          } catch (e) { toast("Couldn't transcribe: " + e.message); }
          setIcon(btn, "mic", "Speak"); btn.disabled = false;
        };
        rec.start();
        btn.classList.add("rec"); setIcon(btn, "stop", "Stop");
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
      qa.append(qaItem(i, st, play));
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

  function conceptTitle(id) { const n = view.data.nodes.find((x) => x.id === id); return n ? n.title : ""; }
  function qaItem(i, st, play) {
    const about = i.node_id ? conceptTitle(i.node_id) : "";
    return h("div", { class: "qa" }, h("div", { class: "spread" },
      h("span", { class: "muted small" }, (i.kind === "checkin" ? "Check-in answer" : "You asked") + (about ? " · " + about : "") + " · " + new Date(i.created_at).toLocaleString()), st),
      h("div", {}, i.text), play);
  }

  // ---------- contents ----------
  let contentsBusy = false;
  async function renderContents() {
    const box = view && view.els && view.els.toc;
    if (!box || contentsBusy) return;
    contentsBusy = true;
    let items = [];
    try { items = await api("courses/" + view.id + "/contents"); } catch { contentsBusy = false; return; }
    contentsBusy = false;
    const byId = {};
    view.data.nodes.forEach((n) => (byId[n.id] = n));
    const cur = player.currentSeg();
    const groups = [];
    let last = null;
    for (const it of items) {
      const concept = byId[it.node_id], mod = concept && byId[concept.parent_id];
      const key = mod ? mod.id : "";
      if (!last || last.key !== key) { last = { key, title: mod ? mod.title : "", items: [] }; groups.push(last); }
      last.items.push({ ...it, concept: concept ? concept.title : "" });
    }
    box.replaceChildren(...groups.map((g) => h("div", { class: "toc-group" },
      g.title ? h("div", { class: "eyebrow" }, g.title) : null,
      ...g.items.map((it) => {
        const now = cur && cur.id === it.id;
        const state = now ? "now" : it.played ? "heard" : it.status === "ready" ? "ready" : "writing";
        const icon = { now: "▶", heard: "✓", ready: "•", writing: "…" }[state];
        return h("button", { class: "toc-item " + state, disabled: it.status !== "ready", title: it.concept,
          onclick: () => { document.body.classList.remove("toc-open"); player.jumpTo(it.id); } },
          h("span", { class: "toc-icon", "aria-hidden": "true" }, icon),
          h("span", { class: "toc-title" }, (it.kind === "answer" ? "Answer: " : it.kind === "feedback" ? "Feedback: " : "") + (it.title || "Untitled")),
          h("span", { class: "toc-dur muted" }, it.duration_ms ? fmt(it.duration_ms / 1000) : state === "writing" ? "writing" : ""));
      }))));
    const nowEl = box.querySelector(".toc-item.now");
    if (nowEl && !box.dataset.scrolled) { nowEl.scrollIntoView({ block: "center" }); box.dataset.scrolled = "1"; }
  }

  // ---------- ask from anywhere ----------
  function openSheet(title, ...content) {
    document.querySelectorAll(".sheet-overlay").forEach((x) => x.remove());
    const ov = h("div", { class: "sheet-overlay" },
      h("div", { class: "sheetbox", role: "dialog", "aria-label": title },
        h("div", { class: "spread" }, h("h2", {}, title), h("button", { class: "quiet small", onclick: () => ov.remove() }, "Close")), ...content));
    ov.addEventListener("click", (e) => { if (e.target === ov) ov.remove(); });
    document.body.append(ov);
    return () => ov.remove();
  }
  function openAsk() {
    if (!view || !view.data || !view.data.can_steer) return;
    const ta = h("textarea", { placeholder: "Ask, or steer: \"go deeper here\", \"I know this\", \"show me where this is in our code\"…" });
    const send = h("button", { class: "primary" }, "Send");
    const close = openSheet("Ask or steer", ta,
      h("div", { class: "row" }, send, micButton((t) => { ta.value = (ta.value ? ta.value + " " : "") + t; ta.focus(); }),
        h("span", { class: "muted small" }, "The answer plays next. Only what's after you changes.")));
    send.onclick = async () => {
      if (!ta.value.trim()) return;
      send.disabled = true;
      try {
        await api("courses/" + view.id + "/ask", { method: "POST", body: { text: ta.value, kind: "ask", segment_id: player.currentId(), offset_ms: Math.round(audio.currentTime * 1000) } });
        close(); toast("Got it. Your answer will play next."); loadInteractions();
      } catch (e) { toast(e.message); send.disabled = false; }
    };
    setTimeout(() => ta.focus(), 50);
  }

  // ---------- icons ----------
  const ICONS = {
    play: '<path d="M8 5.5v13a1 1 0 0 0 1.5.86l10.5-6.5a1 1 0 0 0 0-1.72L9.5 4.64A1 1 0 0 0 8 5.5z"/>',
    pause: '<rect x="6" y="5" width="4.5" height="14" rx="1.2"/><rect x="13.5" y="5" width="4.5" height="14" rx="1.2"/>',
    back: '<path d="M12 5V2L7 6l5 4V7a6 6 0 1 1-6 6H4a8 8 0 1 0 8-8z"/><text x="12" y="16.2" font-size="6.6" font-weight="700" text-anchor="middle" font-family="system-ui,sans-serif">15</text>',
    fwd: '<path d="M12 5V2l5 4-5 4V7a6 6 0 1 0 6 6h2a8 8 0 1 1-8-8z"/><text x="12" y="16.2" font-size="6.6" font-weight="700" text-anchor="middle" font-family="system-ui,sans-serif">15</text>',
    next: '<path d="M6 5.5v13a1 1 0 0 0 1.5.86L17 13.2V18a1 1 0 0 0 2 0V6a1 1 0 0 0-2 0v4.8L7.5 4.64A1 1 0 0 0 6 5.5z"/>',
    ask: '<path d="M4 5a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H9l-4.4 3.5A.6.6 0 0 1 3.6 20V5H4zm4 4.5h8v-1.6H8zm0 3.6h5.5v-1.6H8z"/>',
    mic: '<path d="M12 14a3 3 0 0 0 3-3V5a3 3 0 0 0-6 0v6a3 3 0 0 0 3 3zm5-3a5 5 0 0 1-10 0H5a7 7 0 0 0 6 6.92V21h2v-3.08A7 7 0 0 0 19 11z"/>',
    stop: '<rect x="6" y="6" width="12" height="12" rx="2.5"/>',
    check: '<path d="M9.5 16.2 5.3 12l-1.4 1.4 5.6 5.6L20.1 8.4 18.7 7z"/>',
    trash: '<path d="M9 3h6l1 2h4v2H4V5h4zm-3 6h12l-1 12H7z"/>',
    sound: '<path d="M4 9v6h4l5 4V5L8 9zm12.5 3a4.5 4.5 0 0 0-2.5-4v8a4.5 4.5 0 0 0 2.5-4zM14 3.2v2.1a7 7 0 0 1 0 13.4v2.1a9 9 0 0 0 0-17.6z"/>',
  };
  function icon(name, label) {
    const s = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    s.setAttribute("viewBox", "0 0 24 24"); s.setAttribute("aria-hidden", "true"); s.setAttribute("class", "ico");
    s.innerHTML = ICONS[name];
    return label ? [s, h("span", {}, label)] : s;
  }
  const iconButton = (name, label, attrs = {}) => h("button", { class: "icon", "aria-label": label, title: label, ...attrs }, icon(name));
  function setIcon(btn, name, label) { btn.replaceChildren(icon(name)); btn.setAttribute("aria-label", label); }

  // ---------- teach a pronunciation ----------
  function teachWord(raw) {
    const word = raw.trim().replace(/^[^\p{L}\p{N}]+|[^\p{L}\p{N}]+$/gu, "").replace(/['’]s$/, "");
    if (!word || word.length > 60) return;
    const same = (say) => say.trim().toLowerCase() === word.toLowerCase();
    const input = h("input", { type: "text", value: word, autocomplete: "off", spellcheck: "false", autocapitalize: "off", "aria-label": "How it sounds" });
    const saved = h("p", { class: "muted small", style: "margin:0" });
    const tips = h("div", { class: "row" });
    const status = h("p", { class: "muted small", style: "margin:0" },
      "Spell it as one word, the way it sounds (for example Ehlastyo or Elasteeo), then press play to hear it. Save when it's right.");
    const playOnce = (url) => { const a = new Audio(url); a.play().catch(() => {}); };
    let heard = { say: "", url: "" };
    const hearBtn = iconButton("play", "Hear it", { class: "icon primary" });
    const hear = async () => {
      const say = input.value.trim();
      if (!say) return;
      if (heard.say === say) return playOnce(heard.url);
      hearBtn.disabled = true; hearBtn.replaceChildren(h("span", { class: "spinner" }));
      try { const r = await api("courses/" + view.id + "/pronounce/try", { method: "POST", body: { say } }); heard = { say, url: r.url }; playOnce(r.url); }
      catch (e) { toast(e.message); }
      hearBtn.disabled = false; hearBtn.replaceChildren(icon("play"));
    };
    hearBtn.onclick = hear;
    input.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); hear(); } });
    let close = () => {};
    const saveBtn = h("button", { class: "primary" }, icon("check", "Save"));
    saveBtn.onclick = async () => {
      const say = input.value.trim();
      if (!say) return;
      try {
        if (same(say)) {
          await api("lexicon/" + encodeURIComponent(word), { method: "DELETE" });
          close(); toast("“" + word + "” is back to the voice's own way of saying it."); return;
        }
        const r = await api("lexicon", { method: "PUT", body: { term: word, say, course: view.id, clip: heard.say === say ? heard.url : "" } });
        close(); toast("Saved. “" + word + "” is now always said as “" + say + "”." + (r.revoicing ? " Re-voicing this segment first (about a minute), then the rest; it switches over by itself." : ""), null, null, 9000);
        if (r.revoicing) player.watchRevoice();
      } catch (e) { toast(e.message); }
    };
    api("lexicon").then((l) => {
      const e = l.find((x) => x.term.toLowerCase() === word.toLowerCase());
      if (e) { input.value = e.say; saved.textContent = "Saved now: “" + e.say + "”. Change it, or set it back to “" + word + "” to undo."; }
    }).catch(() => {});

    // Optional: say it, and get spellings to try.
    let rec = null, chunks = [];
    const recBtn = iconButton("mic", "Say it and get suggestions");
    recBtn.onclick = async () => {
      if (rec && rec.state === "recording") { rec.stop(); return; }
      if (!navigator.mediaDevices || !window.MediaRecorder) { status.textContent = "Recording needs the HTTPS address. Typing works anywhere."; return; }
      try {
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        chunks = []; rec = new MediaRecorder(stream);
        rec.ondataavailable = (e) => e.data.size && chunks.push(e.data);
        rec.onstop = async () => {
          stream.getTracks().forEach((t) => t.stop());
          setIcon(recBtn, "mic", "Say it and get suggestions"); recBtn.classList.remove("rec");
          status.replaceChildren(h("span", { class: "spinner" }), " Finding spellings that sound like you…");
          tips.replaceChildren();
          const fd = new FormData();
          fd.append("word", word);
          fd.append("audio", new Blob(chunks, { type: rec.mimeType || "audio/webm" }), "word");
          try {
            const r = await api("courses/" + view.id + "/pronounce", { method: "POST", body: fd });
            status.textContent = "Closest to you first. Tap one to hear it and put it in the box.";
            tips.replaceChildren(...r.options.filter((o) => !same(o.say)).map((o) =>
              h("button", { class: "chip", onclick: () => { input.value = o.say; heard = { say: o.say, url: o.url }; playOnce(o.url); } }, icon("sound"), o.say)));
          } catch (e) { status.textContent = "That didn't work: " + e.message; }
        };
        rec.start(); setIcon(recBtn, "stop", "Stop"); recBtn.classList.add("rec"); status.textContent = "Listening… say “" + word + "”, then press stop.";
      } catch (e) { status.textContent = "Microphone unavailable: " + e.message; }
    };
    close = openSheet("How should “" + word + "” sound?", saved,
      h("div", { class: "row", style: "flex-wrap:nowrap" }, input, hearBtn, recBtn), status, tips,
      h("div", { class: "row" }, saveBtn));
    setTimeout(() => { input.focus(); input.select(); }, 50);
  }

  // ---------- mind map ----------
  function mindMapSection(d) {
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("class", "mindmap");
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", "Mind map of the topic");
    view.els.map = svg;
    view.els.mapDetail = h("div", { class: "panel", hidden: true });
    const zoom = (f) => { const vb = view.mapVB; if (!vb) return; const cx = vb[0] + vb[2] / 2, cy = vb[1] + vb[3] / 2; vb[2] *= f; vb[3] *= f; vb[0] = cx - vb[2] / 2; vb[1] = cy - vb[3] / 2; svg.setAttribute("viewBox", vb.join(" ")); };
    // Drag to pan, pinch or wheel to zoom.
    let drag = null;
    svg.addEventListener("pointerdown", (e) => { if (e.target.closest(".mm-concept")) return; drag = { x: e.clientX, y: e.clientY, vb: view.mapVB.slice() }; svg.setPointerCapture(e.pointerId); });
    svg.addEventListener("pointermove", (e) => {
      if (!drag) return;
      const k = view.mapVB[2] / svg.clientWidth;
      view.mapVB[0] = drag.vb[0] - (e.clientX - drag.x) * k; view.mapVB[1] = drag.vb[1] - (e.clientY - drag.y) * k;
      svg.setAttribute("viewBox", view.mapVB.join(" "));
    });
    svg.addEventListener("pointerup", () => (drag = null));
    svg.addEventListener("wheel", (e) => { e.preventDefault(); zoom(e.deltaY > 0 ? 1.12 : 0.89); }, { passive: false });
    return h("section", { class: "stack" },
      h("div", { class: "spread" }, h("p", { class: "muted small", style: "margin:0;max-width:52ch" },
        "Not a book: what plays next depends on you, and ideas come back from new angles. Darker means you know it better; bigger means you've heard it more. Tap an idea."),
        h("div", { class: "row" }, h("button", { onclick: () => zoom(0.8), "aria-label": "Zoom in" }, "+"), h("button", { onclick: () => zoom(1.25), "aria-label": "Zoom out" }, "−"), h("button", { onclick: () => drawMindMap(true) }, "Fit"))),
      h("div", { class: "mapbox" }, svg), view.els.mapDetail);
  }

  function drawMindMap(refit) {
    const svg = view.els && view.els.map;
    if (!svg) return;
    const d = view.data, NS = "http://www.w3.org/2000/svg";
    const el = (t, a, txt) => { const x = document.createElementNS(NS, t); for (const k in a) x.setAttribute(k, a[k]); if (txt != null) x.textContent = txt; return x; };
    const mods = d.nodes.filter((n) => n.kind === "module");
    const concepts = d.nodes.filter((n) => n.kind === "concept");
    const cur = player.currentSeg();
    const total = Math.max(1, concepts.length);
    const R1 = 230, R2 = Math.max(400, total * 15);
    svg.replaceChildren();
    const g = el("g", {});
    svg.append(g);
    let start = -Math.PI / 2;
    const center = el("g", {});
    center.append(el("circle", { cx: 0, cy: 0, r: 92, class: "mm-root" }));
    const words = (d.course.title || d.course.topic).split(/\s+/);
    const lines = []; let line = "";
    for (const w of words) { if ((line + " " + w).trim().length > 14 && line) { lines.push(line); line = w; } else line = (line + " " + w).trim(); }
    if (line) lines.push(line);
    lines.slice(0, 4).forEach((l, i) => center.append(el("text", { x: 0, y: (i - (Math.min(lines.length, 4) - 1) / 2) * 26 + 8, class: "mm-root-text", "text-anchor": "middle" }, l)));
    for (const m of mods) {
      const kids = concepts.filter((n) => n.parent_id === m.id);
      const span = (Math.max(1, kids.length) / total) * Math.PI * 2;
      const mid = start + span / 2;
      const mx = Math.cos(mid) * R1, my = Math.sin(mid) * R1;
      g.append(el("line", { x1: 0, y1: 0, x2: mx, y2: my, class: "mm-edge" }));
      kids.forEach((n, i) => {
        const a = start + span * ((i + 0.5) / Math.max(1, kids.length));
        const x = Math.cos(a) * R2, y = Math.sin(a) * R2;
        g.append(el("path", { d: "M" + mx + "," + my + " Q" + Math.cos(a) * (R1 + 120) + "," + Math.sin(a) * (R1 + 120) + " " + x + "," + y, class: "mm-edge" }));
        const node = el("g", { class: "mm-concept" + (cur && cur.node_id === n.id ? " now" : "") + (view.mapSel === n.id ? " sel" : ""), tabindex: 0, role: "button", "aria-label": n.title });
        const r = 10 + Math.min(18, n.covered * 3);
        node.append(el("circle", { cx: x, cy: y, r, style: "fill-opacity:" + (0.15 + 0.85 * n.mastery).toFixed(2) }));
        const deg = (a * 180) / Math.PI, flip = Math.cos(a) < 0;
        const tx = x + Math.cos(a) * (r + 10), ty = y + Math.sin(a) * (r + 10);
        const label = n.title.length > 42 ? n.title.slice(0, 40) + "…" : n.title;
        node.append(el("text", { x: tx, y: ty, dy: "0.35em", "text-anchor": flip ? "end" : "start", transform: "rotate(" + (flip ? deg + 180 : deg) + " " + tx + " " + ty + ")" }, label));
        node.addEventListener("click", () => selectConcept(n));
        node.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); selectConcept(n); } });
        g.append(node);
      });
      const mnode = el("g", { class: "mm-module" });
      mnode.append(el("circle", { cx: mx, cy: my, r: 16 }));
      const ml = m.title.length > 26 ? m.title.slice(0, 24) + "…" : m.title;
      mnode.append(el("text", { x: mx, y: my - 26, "text-anchor": "middle" }, ml));
      g.append(mnode);
      start += span;
    }
    g.append(center);
    if (refit || !view.mapVB) {
      // Fit what was actually drawn (labels included), with a little margin.
      let bb = null;
      try { bb = g.getBBox(); } catch {}
      const ext = R2 + 470;
      view.mapVB = bb && bb.width ? [bb.x - 24, bb.y - 24, bb.width + 48, bb.height + 48] : [-ext, -ext, ext * 2, ext * 2];
    }
    svg.setAttribute("viewBox", view.mapVB.join(" "));
  }

  async function selectConcept(n) {
    view.mapSel = n.id; drawMindMap();
    const box = view.els.mapDetail;
    box.hidden = false;
    const can = view.data.can_steer;
    const notesBox = h("div", { class: "small" });
    box.replaceChildren(h("div", { class: "spread" }, h("h2", {}, n.title), h("button", { class: "quiet small", onclick: () => { box.hidden = true; view.mapSel = null; drawMindMap(); } }, "Close")),
      n.summary ? h("p", { class: "muted small", style: "margin:0" }, n.summary) : null,
      h("div", { class: "row small" }, h("span", {}, "Mastery " + Math.round(n.mastery * 100) + "%"), h("span", { class: "muted" }, n.covered ? "heard " + n.covered + "×, last " + new Date(n.last_covered_at).toLocaleDateString() : "not covered yet"),
        n.correct_recalls ? h("span", { class: "muted" }, n.correct_recalls + " correct recall" + (n.correct_recalls > 1 ? "s" : "")) : null),
      can ? h("div", { class: "row" },
        h("button", { class: "primary", onclick: async () => { try { await api("courses/" + view.id + "/focus", { method: "POST", body: { node_id: n.id } }); toast("Next up: " + n.title + ". It plays after the current segment."); player.refreshQueue(); } catch (e) { toast(e.message); } } }, "Teach this next"),
        h("button", { onclick: () => startQuiz(n.id) }, "Quiz me on this")) : null,
      notesBox);
    if (can) {
      try {
        const notes = (await api("courses/" + view.id + "/notes")).filter((x) => x.node_id === n.id);
        if (notes.length) notesBox.replaceChildren(h("div", { class: "eyebrow" }, "What Learn knows about you here"), ...notes.map((x) => h("div", { class: x.resolved ? "muted" : "" }, x.kind + ": " + x.text + (x.resolved ? " (resolved)" : ""))));
      } catch {}
    }
    box.scrollIntoView({ behavior: "smooth", block: "nearest" });
  }

  // ---------- quiz ----------
  async function startQuiz(nodeId) {
    try {
      const q = await api("courses/" + view.id + "/quizzes", { method: "POST", body: { node_id: nodeId || "" } });
      view.quizId = q.id;
      if (view.show) view.show("quiz");
    } catch (e) { toast(e.message); }
  }

  async function renderQuizTab() {
    const box = view.els.quiz;
    if (!box) return;
    let list = [];
    try { list = await api("courses/" + view.id + "/quizzes"); } catch (e) { box.replaceChildren(h("p", {}, e.message)); return; }
    const active = list.find((q) => q.id === view.quizId) || list.find((q) => q.status === "writing" || q.status === "ready" || q.status === "grading");
    const head = h("div", { class: "spread" }, h("div", {}, h("h2", {}, "Quiz"), h("p", { class: "muted small", style: "margin:4px 0 0" }, "Five questions on what you've heard, leaning on what you find hardest. Results update the mind map.")),
      h("button", { class: "primary", onclick: () => startQuiz("") }, "New quiz"));
    const parts = [head];
    if (active) parts.push(quizView(active));
    const past = list.filter((q) => q.status === "done" && (!active || q.id !== active.id)).slice(0, 10);
    if (past.length) parts.push(h("section", { class: "panel" }, h("h2", {}, "Earlier quizzes"),
      ...past.map((q) => h("button", { class: "quiet", style: "text-align:left", onclick: () => { view.quizId = q.id; renderQuizTab(); } },
        new Date(q.created_at).toLocaleString() + " · " + Math.round(q.score * 100) + "%"))));
    box.replaceChildren(...parts);
    if (active && (active.status === "writing" || active.status === "grading")) setTimeout(() => { if (view && view.tab === "quiz") renderQuizTab(); }, 4000);
  }

  function quizView(q) {
    if (q.status === "writing") return h("div", { class: "panel" }, h("p", {}, h("span", { class: "spinner" }), " Writing your quiz…"));
    if (q.status === "failed") return h("div", { class: "panel" }, h("p", {}, "Couldn't write this quiz: " + q.error));
    const qs = q.questions || [], results = q.results || [], answers = q.answers || [];
    const done = q.status === "done" || q.status === "grading";
    const picks = qs.map((_, i) => (answers[i] !== undefined ? answers[i] : null));
    const form = h("form", { class: "stack" });
    qs.forEach((qq, i) => {
      const r = results[i];
      const card = h("div", { class: "panel quizq" + (r ? " " + r.verdict : "") },
        h("div", { class: "eyebrow" }, (i + 1) + " of " + qs.length + (qq.concept ? " · " + qq.concept : "")),
        h("p", { style: "margin:0;font-family:var(--font-read);font-size:1.1rem" }, qq.q));
      if (qq.type === "mc") {
        const opts = h("div", { class: "stack", style: "gap:8px" });
        qq.options.forEach((o, k) => {
          const b = h("button", { type: "button", class: "opt" + (picks[i] === k ? " picked" : "") + (done && k === qq.answer ? " right" : "") + (done && picks[i] === k && k !== qq.answer ? " wrongpick" : ""), disabled: done,
            onclick: () => { picks[i] = k; opts.querySelectorAll(".opt").forEach((x, j) => x.classList.toggle("picked", j === k)); } }, o);
          opts.append(b);
        });
        card.append(opts);
      } else {
        const ta = h("textarea", { placeholder: "Answer in a sentence or two", disabled: done });
        ta.value = typeof picks[i] === "string" ? picks[i] : "";
        ta.addEventListener("input", () => (picks[i] = ta.value));
        card.append(h("div", { class: "row", style: "flex-wrap:nowrap;align-items:start" }, ta, done ? null : micButton((t) => { ta.value = (ta.value ? ta.value + " " : "") + t; picks[i] = ta.value; })));
        if (done && qq.model_answer) card.append(h("p", { class: "muted small", style: "margin:0" }, "A good answer: " + qq.model_answer));
      }
      if (r) card.append(h("p", { class: "small", style: "margin:0" }, h("b", {}, r.verdict === "pending" ? "Grading…" : r.verdict[0].toUpperCase() + r.verdict.slice(1) + ". "), r.feedback || ""));
      form.append(card);
    });
    if (!done) {
      form.append(h("div", { class: "row" }, h("button", { class: "primary", type: "submit" }, "Check my answers")));
      form.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        if (picks.some((p) => p === null || p === "")) { toast("Answer every question first (a guess is fine)."); return; }
        try { await api("courses/" + view.id + "/quizzes/" + q.id + "/answers", { method: "POST", body: { answers: picks } }); view.quizId = q.id; renderQuizTab(); refreshCourse(false); }
        catch (e) { toast(e.message); }
      });
    } else {
      form.prepend(h("div", { class: "panel" }, h("b", {}, q.status === "grading" ? "Marking your written answers…" : "Score: " + Math.round(q.score * 100) + "%"),
        h("p", { class: "muted small", style: "margin:0" }, "Correct answers raise mastery; a concept counts as mastered after two correct recalls on different days.")));
    }
    return form;
  }

  // ---------- all questions ----------
  async function renderAllQuestions() {
    const box = view.els.allq;
    if (!box) return;
    let is = [];
    try { is = await api("courses/" + view.id + "/interactions?all=1"); } catch (e) { box.replaceChildren(h("p", {}, e.message)); return; }
    const filter = h("input", { type: "text", placeholder: "Filter your questions", autocomplete: "off" });
    const list = h("section", { class: "panel" });
    const draw = () => {
      const f = filter.value.toLowerCase();
      const rows = is.filter((i) => !f || i.text.toLowerCase().includes(f) || conceptTitle(i.node_id).toLowerCase().includes(f));
      list.replaceChildren(h("h2", {}, rows.length + " question" + (rows.length === 1 ? "" : "s")),
        ...(rows.length ? rows.map((i) => qaItem(i, i.status === "pending" ? h("span", { class: "pill busy" }, "answering") : h("span", { class: "pill" + (i.status === "failed" ? " bad" : " live") }, i.verdict || i.status),
          i.answer_segment_id ? h("button", { class: "quiet small", onclick: () => { player.jumpTo(i.answer_segment_id); view.show("listen"); } }, "Play answer") : null)) : [h("p", { class: "muted small" }, "Nothing yet. Ask from the Listen tab, typed or spoken.")]));
    };
    filter.addEventListener("input", draw);
    box.replaceChildren(h("p", { class: "muted small", style: "margin:0" }, "Everything you've asked or answered, tagged to the idea it was about. Each one feeds what Learn knows about you."), filter, list);
    draw();
  }

  // ---------- about you ----------
  async function renderAboutYou() {
    const box = view.els.you;
    if (!box) return;
    let notes = [];
    try { notes = await api("courses/" + view.id + "/notes"); } catch {}
    const prof = view.data.course.profile || {};
    const listOf = (k, label) => Array.isArray(prof[k]) && prof[k].length ? h("div", {}, h("div", { class: "eyebrow" }, label), h("ul", { class: "small" }, ...prof[k].map((x) => h("li", {}, typeof x === "string" ? x : JSON.stringify(x))))) : null;
    const profile = h("section", { class: "panel" }, h("h2", {}, "Your profile on this topic"),
      prof.level ? h("p", { style: "margin:0" }, h("b", {}, "Level: "), prof.level) : null,
      prof.pitch ? h("p", { class: "muted small", style: "margin:0" }, "How I teach you: " + prof.pitch) : null,
      listOf("knows", "You know"), listOf("gaps", "Gaps"), listOf("misconceptions", "Watch-outs"),
      !Object.keys(prof).length ? h("p", { class: "muted small" }, "Builds up as you listen, ask and answer.") : null);
    const kinds = { misconception: "Misconceptions", gap: "Gaps", strength: "Strengths", interest: "Interests", preference: "Preferences", note: "Notes" };
    const groups = Object.keys(kinds).map((k) => {
      const ns = notes.filter((n) => n.kind === k);
      if (!ns.length) return null;
      return h("section", { class: "panel" }, h("h2", {}, kinds[k]),
        ...ns.map((n) => h("div", { class: "qa" },
          h("div", { class: n.resolved ? "muted" : "" }, n.text, n.resolved ? " (resolved)" : ""),
          h("div", { class: "spread" }, h("span", { class: "muted small" }, (conceptTitle(n.node_id) || "general") + " · from " + (n.source || "listening") + " · " + new Date(n.created_at).toLocaleDateString()),
            h("div", { class: "row" },
              h("button", { class: "quiet small", onclick: async () => { await api("courses/" + view.id + "/notes/" + n.id, { method: "PATCH", body: { resolved: !n.resolved } }); renderAboutYou(); } }, n.resolved ? "Reopen" : "Resolved"),
              h("button", { class: "quiet small danger", onclick: async () => { await api("courses/" + view.id + "/notes/" + n.id, { method: "PATCH", body: { delete: true } }); renderAboutYou(); } }, "Wrong, remove"))))));
    });
    box.replaceChildren(h("p", { class: "muted small", style: "margin:0" }, "What Learn has picked up about you from your questions, check-ins and quizzes. Only the notes relevant to what's playing are used each time. Remove anything that's wrong."), profile, ...groups.filter(Boolean),
      notes.length ? null : h("p", { class: "muted small" }, "No notes yet."));
  }

  // ---------- settings ----------
  function settingsSection(d) {
    const c = d.course;
    const memberInput = h("input", { type: "text", placeholder: "their Tailscale login, e.g. friend@example.com", autocomplete: "off" });
    const members = h("div", { class: "small muted" }, (d.members || []).length ? "Listening: " + d.members.join(", ") : "Not shared yet.");
    const lexBox = h("div", { class: "field" });
    const drawLex = async () => {
      let lex = [];
      try { lex = await api("lexicon"); } catch {}
      lexBox.replaceChildren(h("span", { style: "font-weight:500" }, "Pronunciations (all your topics)"),
        lex.length ? null : h("span", { class: "muted small" }, "None yet. Select a word in the transcript (double-click, or press and hold on a phone), then “Fix how it's said”."),
        ...lex.map((l) => h("div", { class: "row small" }, h("b", {}, l.term), "→", h("span", { style: "flex:1" }, l.say),
          iconButton("play", "Hear " + l.say, { class: "icon quiet", onclick: async () => { try { const r = await api("courses/" + d.course.id + "/pronounce/try", { method: "POST", body: { say: l.say } }); new Audio(r.url).play().catch(() => {}); } catch (e) { toast(e.message); } } }),
          iconButton("trash", "Remove " + l.term, { class: "icon quiet danger", onclick: async () => { await api("lexicon/" + encodeURIComponent(l.term), { method: "DELETE" }); drawLex(); } }))));
    };
    drawLex();
    const box = h("details", { class: "panel" }, h("summary", {}, h("b", {}, "Settings")),
      h("div", { class: "field" }, h("span", { style: "font-weight:500" }, "Voice (applies to new segments)"), voicePicker(c.voice_id, "s-voice")),
      providerFields(c.provider, c.model),
      h("div", { class: "row" },
        h("button", { onclick: async () => {
          const v = box.querySelector("input[name=s-voice]:checked");
          await api("courses/" + c.id, { method: "PATCH", body: { voice_id: v ? v.value : c.voice_id, provider: $("f-provider").value, model: $("f-model").value } });
          toast("Saved."); refreshCourse(true);
        } }, "Save"),
        h("button", { onclick: async () => { await api("courses/" + c.id, { method: "PATCH", body: { paused: !c.paused } }); refreshCourse(true); } }, c.paused ? "Resume generating" : "Pause generating"),
        h("button", { onclick: async () => { await api("courses/" + c.id, { method: "PATCH", body: { research: !c.research } }); toast(c.research ? "The teacher won't read your code or GitHub for this topic." : "The teacher can now read your code and GitHub (read-only)."); refreshCourse(true); } },
          c.research ? "Stop reading my code" : "Let it read my code"),
        h("button", { onclick: async (ev) => {
          const b = ev.currentTarget;
          if (!b.dataset.armed) { b.dataset.armed = "1"; b.textContent = "Tap again: re-plan the map (unheard segments are dropped)"; return; }
          try { await api("courses/" + c.id + "/rebuild", { method: "POST" }); toast("Re-planning the map" + (c.research ? " from your code and GitHub" : "") + ". New segments follow in a few minutes."); refreshCourse(true); }
          catch (e) { toast(e.message); }
        } }, c.research ? "Rebuild from my code" : "Rebuild the map")),
      lexBox,
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
    const fix = h("button", { class: "quiet small", "aria-pressed": "false" }, "Fix a word");
    fix.onclick = () => {
      const on = fix.getAttribute("aria-pressed") !== "true";
      fix.setAttribute("aria-pressed", String(on)); fix.textContent = on ? "Tap the word to fix" : "Fix a word";
      view.els.reader.classList.toggle("fixing", on);
    };
    view.els.fix = fix;
    return h("section", { class: "stack" }, h("div", { class: "spread" }, h("h2", {}, "Now"), h("div", { class: "row" }, d.can_steer ? fix : null,
      d.can_steer ? h("button", { class: "quiet small", title: "Write this segment again from your code", onclick: () => player.rewriteCurrent() }, "Rewrite") : null, view.els.modes)), view.els.reader);
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
      audio.src = "api/courses/" + courseId + "/audio/" + cur.id + "?v=" + encodeURIComponent(cur.audio_v || "");
      audio.playbackRate = SPEEDS[speedIdx];
      const at = resumeAt; resumeAt = 0;
      audio.onloadedmetadata = () => { if (at > 0 && at < audio.duration - 1) audio.currentTime = at; audio.playbackRate = SPEEDS[speedIdx]; };
      renderReader(); renderBar(); setMediaSession(); renderContents();
      if (view && view.tab === "map") drawMindMap();
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
        cur = g; resumeAt = 0; load(mode !== "read"); refreshQueueSoon();
        if (mode !== "read") save();
        scrollTo({ top: 0, behavior: "smooth" });
      } catch (e) { toast(e.message); }
    }

    let lastOwnSave = "", lastSeen = "";
    async function save(finishedId, finished) {
      if (!cur || !courseId) return;
      lastSave = Date.now();
      try {
        const r = await api("courses/" + courseId + "/playback", { method: "PUT", body: {
          segment_id: cur.id, offset_ms: finished ? Math.round((audio.duration || 0) * 1000) : Math.round(audio.currentTime * 1000),
          rate: SPEEDS[speedIdx], finished: finished ? finishedId : "", device } });
        if (r && r.updated_at) lastOwnSave = r.updated_at;
      } catch {}
    }

    // Handoff between devices. A background poll only notes where another device is; this one jumps
    // there when you come back to it or press play, and a device that's playing steps aside at once.
    let remote = null, steppedAside = "";
    const newer = (pb) => pb && pb.segment_id && pb.device !== device && pb.updated_at > lastOwnSave && pb.updated_at > lastSeen;
    async function sync(trigger) {
      if (!courseId || !attached) return;
      let pb;
      try { pb = await api("courses/" + courseId + "/playback"); } catch { return; }
      if (!newer(pb)) return;
      remote = pb;
      if (!audio.paused) {
        audio.pause();
        if (steppedAside !== pb.updated_at) {
          steppedAside = pb.updated_at;
          toast("You're listening on your " + deviceLabel(pb.device) + ", so this one paused.", "Play here", () => takeOver(pb, true), 10000);
        }
        return;
      }
      if (trigger === "visible") {
        await takeOver(pb, false);
        toast("Picked up where you left off on your " + deviceLabel(pb.device) + ".");
      } else {
        $("bar-sub").textContent = "Playing on your " + deviceLabel(pb.device) + " · press Play to continue here";
      }
    }
    async function takeOver(pb, play) {
      lastSeen = pb.updated_at; remote = null;
      if (cur && pb.segment_id === cur.id) {
        audio.currentTime = (pb.offset_ms || 0) / 1000;
        if (play) audio.play().catch(() => {});
        return;
      }
      try {
        const g = await api("courses/" + courseId + "/segments/" + pb.segment_id);
        cur = g; resumeAt = (pb.offset_ms || 0) / 1000;
        load(play); refreshQueueSoon();
      } catch (e) { toast(e.message); }
    }
    // When the segment you're on is re-voiced (a pronunciation fix), swap the new audio in at the same word.
    async function checkFresh() {
      if (!cur || !courseId) return false;
      let g;
      try { g = await api("courses/" + courseId + "/segments/" + cur.id); } catch { return false; }
      if (g.status !== "ready" || !g.audio_v || g.audio_v === cur.audio_v) return false;
      const i = wordAt(audio.currentTime), playing = !audio.paused, rewritten = g.script !== cur.script;
      cur = g; words = Array.isArray(g.words) ? g.words : [];
      const at = rewritten ? 0 : i >= 0 && words[i] ? words[i].s : audio.currentTime; // a rewrite starts from the top
      audio.src = "api/courses/" + courseId + "/audio/" + cur.id + "?v=" + encodeURIComponent(cur.audio_v);
      audio.onloadedmetadata = () => { audio.currentTime = Math.min(at, Math.max(0, audio.duration - 1)); audio.playbackRate = SPEEDS[speedIdx]; if (playing) audio.play().catch(() => {}); };
      renderReader();
      toast(rewritten ? "Here's the rewritten version." : "Now playing with your pronunciation fix.", null, null, 4000);
      renderContents();
      return true;
    }
    let freshPoll = null;
    function watchRevoice() {
      clearInterval(freshPoll);
      const until = Date.now() + 6 * 60000;
      freshPoll = setInterval(async () => { if (Date.now() > until || (await checkFresh())) clearInterval(freshPoll); refreshQueue(); }, 5000);
    }
    function rewriteCurrent() {
      if (!cur || !view || !view.data || !view.data.can_steer) return;
      const seg = cur;
      const ta = h("textarea", { placeholder: "Optional: what should change? e.g. \"more about how the scanner actually works in our code\"" });
      const go = h("button", { class: "primary" }, "Rewrite");
      const close = openSheet("Rewrite “" + (seg.title || "this segment") + "”", h("p", { class: "muted small", style: "margin:0" },
        "The teacher writes this segment again from your code, keeping its place in the course. Keep listening: the new version swaps in when it's ready (a couple of minutes) and starts from the top."), ta, h("div", { class: "row" }, go));
      go.onclick = async () => {
        go.disabled = true;
        try {
          await api("courses/" + courseId + "/segments/" + seg.id + "/rewrite", { method: "POST", body: { note: ta.value } });
          close(); toast("Rewriting from your code. It'll swap in by itself.");
          clearInterval(freshPoll);
          const until = Date.now() + 10 * 60000;
          freshPoll = setInterval(async () => { if (Date.now() > until || (await checkFresh())) clearInterval(freshPoll); }, 8000);
        } catch (e) { toast(e.message); go.disabled = false; }
      };
      setTimeout(() => ta.focus(), 50);
    }
    setInterval(checkFresh, 30000);

    function play() {
      if (newer(remote)) return takeOver(remote, true);
      audio.play().catch((e) => toast(e.message));
    }
    document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible") sync("visible"); });
    addEventListener("focus", () => sync("visible"));
    setInterval(() => sync("poll"), 15000);

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
      requestAnimationFrame(tick);
    }
    requestAnimationFrame(tick);

    document.addEventListener("click", (e) => {
      const i = e.target && e.target.dataset && e.target.dataset.i;
      if (i == null || !words[+i]) return;
      if (view && view.els && view.els.reader && view.els.reader.classList.contains("fixing")) {
        view.els.fix.click();
        teachWord(words[+i].w);
        return;
      }
      // A double-click selects the word to fix it; wait a moment so it doesn't also jump the audio.
      if (e.detail > 1) { clearTimeout(seekTimer); return; }
      clearTimeout(seekTimer);
      seekTimer = setTimeout(() => {
        if (String(getSelection() || "").trim()) return;
        audio.currentTime = words[+i].s + 0.01; userScrolled = false; $("back").hidden = true;
        if (audio.paused && mode !== "read") audio.play().catch(() => {});
      }, 280);
    });
    let seekTimer = null;

    // Select a word (or a short phrase) in the transcript to fix how it's said.
    const fixPop = h("button", { class: "fixpop", hidden: true }, icon("sound", "Fix how it's said"));
    document.body.append(fixPop);
    let selected = "";
    fixPop.addEventListener("pointerdown", (e) => e.preventDefault()); // keep the selection
    fixPop.onclick = () => { fixPop.hidden = true; const w = selected; getSelection().removeAllRanges(); teachWord(w); };
    document.addEventListener("selectionchange", () => {
      const sel = getSelection(), r = view && view.els && view.els.reader;
      const text = String(sel || "").trim().replace(/\s+/g, " ");
      if (!r || !view.data || !view.data.can_steer || !text || text.length > 60 || text.split(" ").length > 4 || !sel.rangeCount || !r.contains(sel.anchorNode)) { fixPop.hidden = true; return; }
      selected = text;
      const rc = sel.getRangeAt(0).getBoundingClientRect();
      fixPop.hidden = false;
      fixPop.style.left = Math.max(8, Math.min(innerWidth - fixPop.offsetWidth - 8, rc.left + rc.width / 2 - fixPop.offsetWidth / 2)) + "px";
      fixPop.style.top = Math.max(8, rc.top - fixPop.offsetHeight - 10) + "px";
    });
    addEventListener("scroll", () => { fixPop.hidden = true; }, { passive: true });
    const manual = () => { if (!audio.paused && mode === "both") { userScrolled = true; $("back").hidden = false; } };
    addEventListener("wheel", manual, { passive: true });
    addEventListener("touchmove", manual, { passive: true });
    $("back").onclick = () => { userScrolled = false; $("back").hidden = true; if (spans[lit]) spans[lit].scrollIntoView({ block: "center", behavior: "smooth" }); };

    audio.addEventListener("ended", () => advance(true));
    audio.addEventListener("timeupdate", () => { if (!audio.paused && Date.now() - lastSave > 10000) save(); });
    audio.addEventListener("play", () => { setIcon($("play"), "pause", "Pause"); if (mode === "read") setMode("both"); save(); });
    audio.addEventListener("pause", () => { setIcon($("play"), "play", "Play"); save(); });
    audio.addEventListener("error", () => { if (cur) toast("Couldn't play this segment; skipping it.", null, null, 4000); setTimeout(() => advance(true), 1500); });
    document.addEventListener("visibilitychange", () => { if (document.visibilityState === "hidden") save(); });

    $("play").onclick = () => { if (!cur) return; audio.paused ? play() : audio.pause(); };

    // Keyboard: space or k play/pause, arrows or j/l skip 15s, up/down volume, n next, [ ] speed,
    // m mute, a ask, ? the list.
    const setSpeed = (i) => { speedIdx = i; audio.playbackRate = SPEEDS[i]; store.set("speed", SPEEDS[i]); $("speed").textContent = SPEEDS[i] + "×"; toast("Speed " + SPEEDS[i] + "×", null, null, 1200); };
    const ordered = SPEEDS.map((v, i) => [v, i]).sort((x, y) => x[0] - y[0]);
    const stepSpeed = (d) => { const k = ordered.findIndex(([, i]) => i === speedIdx); const n = ordered[Math.max(0, Math.min(ordered.length - 1, k + d))]; setSpeed(n[1]); };
    const setVol = (v) => { audio.muted = false; audio.volume = Math.max(0, Math.min(1, Math.round(v * 10) / 10)); store.set("volume", audio.volume); toast("Volume " + Math.round(audio.volume * 100) + "%", null, null, 1200); };
    { const v = store.get("volume", null); if (typeof v === "number" && v >= 0 && v <= 1) audio.volume = v; }
    document.addEventListener("keydown", (e) => {
      if (e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented) return;
      const t = e.target;
      if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
      if (document.querySelector(".sheet-overlay")) { if (e.key === "Escape") document.querySelectorAll(".sheet-overlay").forEach((x) => x.remove()); return; }
      const k = e.key;
      const act = {
        " ": () => $("play").click(), k: () => $("play").click(),
        ArrowLeft: () => $("rew").click(), j: () => $("rew").click(),
        ArrowRight: () => (e.shiftKey ? $("next").click() : $("fwd").click()), l: () => $("fwd").click(),
        ArrowUp: () => setVol(audio.volume + 0.1), ArrowDown: () => setVol(audio.volume - 0.1),
        n: () => $("next").click(), N: () => $("next").click(),
        "]": () => stepSpeed(1), "[": () => stepSpeed(-1),
        m: () => { audio.muted = !audio.muted; toast(audio.muted ? "Muted" : "Sound on", null, null, 1200); },
        a: () => openAsk(), "?": () => showKeys(),
      }[k];
      if (!act || !cur && !"a?".includes(k)) return;
      if (t && t.tagName === "BUTTON" && (k === " " || k === "Enter")) return; // let a focused button do its own thing
      e.preventDefault(); act();
    });
    function showKeys() {
      const row = (key, what) => h("div", { class: "spread", style: "padding:4px 0;border-bottom:1px solid var(--line)" }, h("span", {}, what), h("kbd", {}, key));
      openSheet("Keyboard shortcuts",
        row("Space or K", "Play / pause"), row("← or J", "Back 15 seconds"), row("→ or L", "Forward 15 seconds"),
        row("Shift → or N", "Next segment"), row("↑ / ↓", "Volume up / down"), row("M", "Mute"),
        row("[ / ]", "Slower / faster"), row("A", "Ask or steer"), row("Esc", "Close a panel"), row("?", "This list"));
    }
    $("rew").onclick = () => { audio.currentTime = Math.max(0, audio.currentTime - 15); };
    $("fwd").onclick = () => { audio.currentTime = Math.min(audio.duration || 0, audio.currentTime + 15); };
    $("next").onclick = () => advance(!audio.paused);
    $("ask").onclick = () => openAsk();
    $("speed").textContent = SPEEDS[speedIdx] + "×";
    $("speed").onclick = () => { speedIdx = (speedIdx + 1) % SPEEDS.length; audio.playbackRate = SPEEDS[speedIdx]; store.set("speed", SPEEDS[speedIdx]); $("speed").textContent = SPEEDS[speedIdx] + "×"; };
    let seeking = false;
    $("seek").oninput = () => { seeking = true; };
    $("seek").onchange = () => { if (audio.duration) audio.currentTime = (+$("seek").value / 1000) * audio.duration; seeking = false; };

    return { attach, detach, refreshQueue, jumpTo, setMode, watchRevoice, rewriteCurrent, currentId: () => (cur ? cur.id : ""), currentSeg: () => cur };
  })();

  // ---------- boot ----------
  (async () => {
    try { me = await api("me"); } catch (e) { app.replaceChildren(h("p", {}, "Can't reach Learn: " + e.message)); return; }
    try { voices = await api("voices"); } catch { voices = []; }
    route();
  })();
})();
