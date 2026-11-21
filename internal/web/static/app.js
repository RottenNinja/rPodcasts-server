// The few behaviours the pages need, plus the footer player that survives navigation.
"use strict";

document.addEventListener("DOMContentLoaded", () => {
  if (window.htmx) htmx.config.responseHandling = [{ code: "204", swap: false }, { code: "...", swap: true }];
  pageReady();
  player.restore();
});
document.addEventListener("htmx:afterSettle", (e) => { if (e.detail.target && e.detail.target.id === "page") pageReady(); });

// A list opened on the Status page stays open across its polls.
let openLists = [];
document.addEventListener("htmx:beforeSwap", (e) => {
  if (e.detail.target && e.detail.target.id === "status-live") openLists = [...e.detail.target.querySelectorAll(".more input:checked")].map((i) => i.id);
});
document.addEventListener("htmx:afterSwap", () => { openLists.splice(0).forEach((id) => { const i = document.getElementById(id); if (i) i.checked = true; }); });

function pageReady() {
  const page = document.getElementById("page");
  if (!page) return;
  if (page.dataset.title) document.title = page.dataset.title;
  document.querySelectorAll(".navlink[data-nav]").forEach((a) => {
    if (a.dataset.nav === page.dataset.nav) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  });
  document.querySelectorAll("[data-sortable] > li[data-id]").forEach((li) => li.setAttribute("draggable", "true"));
  const toasts = document.getElementById("toasts");
  if (toasts && toasts.children.length) setTimeout(() => { toasts.innerHTML = ""; }, 7000);
  showTab();
  loadChapters();
  player.sync();
  player.mark();
}

const post = (url, data) => {
  const fd = new FormData();
  Object.entries(data).forEach(([k, v]) => fd.append(k, v));
  return fetch(url, { method: "POST", body: fd, credentials: "same-origin" });
};
const reloadPage = () => htmx.ajax("GET", location.pathname + location.search, { target: "#page", swap: "outerHTML" });
const clock = (s) => {
  s = Math.max(0, Math.floor(s || 0));
  const h = Math.floor(s / 3600), m = Math.floor(s / 60) % 60, x = String(s % 60).padStart(2, "0");
  return h ? `${h}:${String(m).padStart(2, "0")}:${x}` : `${m}:${x}`;
};
const store = {
  get(k) { try { return JSON.parse(localStorage.getItem(k)); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* storage may be blocked */ } },
};

document.addEventListener("click", (e) => {
  const el = e.target.closest("[data-confirm]");
  if (el && !window.confirm(el.dataset.confirm)) { e.preventDefault(); e.stopImmediatePropagation(); }
}, true);

document.addEventListener("change", (e) => {
  const el = e.target.closest("[data-autosubmit]");
  if (el && el.form) el.form.requestSubmit();
  if (e.target.matches("[data-select-all]")) {
    e.target.closest("form").querySelectorAll('input[name="id"]').forEach((c) => { c.checked = e.target.checked; });
  }
});

document.addEventListener("submit", (e) => {
  const by = e.submitter;
  player.marked(e.target, by && by.name === "action" ? by.value : new FormData(e.target).get("action"));
});

// Close open menus when clicking elsewhere.
document.addEventListener("click", (e) => {
  document.querySelectorAll("details.menu[open]").forEach((d) => { if (!d.contains(e.target)) d.open = false; });
});

// Drag to reorder: posts the moved id, the target and before/after.
let dragged = null;
document.addEventListener("dragstart", (e) => {
  const li = e.target.closest && e.target.closest("[data-sortable] > li[data-id]");
  if (!li) return;
  dragged = li;
  li.classList.add("dragging");
  e.dataTransfer.effectAllowed = "move";
});
document.addEventListener("dragover", (e) => {
  const li = dragged && e.target.closest("[data-sortable] > li[data-id]");
  if (!li || li === dragged || li.parentElement !== dragged.parentElement) return;
  e.preventDefault();
  const r = li.getBoundingClientRect();
  const after = e.clientY > r.top + r.height / 2;
  li.parentElement.querySelectorAll(".drop-before, .drop-after").forEach((x) => x.classList.remove("drop-before", "drop-after"));
  li.classList.add(after ? "drop-after" : "drop-before");
});
document.addEventListener("drop", async (e) => {
  const li = dragged && e.target.closest("[data-sortable] > li[data-id]");
  if (!li || li === dragged) return;
  e.preventDefault();
  const after = li.classList.contains("drop-after");
  const list = li.parentElement;
  list.insertBefore(dragged, after ? li.nextSibling : li);
  const res = await post(list.dataset.sortable, { id: dragged.dataset.id, target: li.dataset.id, place: after ? "after" : "before" });
  if (!res.ok) reloadPage();
});
document.addEventListener("dragend", () => {
  document.querySelectorAll(".dragging, .drop-before, .drop-after").forEach((x) => x.classList.remove("dragging", "drop-before", "drop-after"));
  dragged = null;
});

// Filter helpers: the options list for $channel/#tag, inserted at the cursor.
document.addEventListener("click", (e) => {
  const opt = e.target.closest("[data-options]");
  if (opt) {
    const box = opt.closest(".filter-field").querySelector(".options-box");
    box.hidden = false;
    htmx.ajax("GET", "/filter/options?key=" + opt.dataset.options, { target: box, swap: "innerHTML" });
    return;
  }
  const ins = e.target.closest("[data-insert]");
  if (ins) {
    const field = ins.closest(".filter-field");
    const input = field.querySelector("[data-filter-input]");
    const pos = input.selectionStart ?? input.value.length;
    const before = input.value.slice(0, pos), after = input.value.slice(pos);
    const glue = before && !/\s$/.test(before) ? " " : "";
    input.value = before + glue + ins.dataset.insert + (after && !/^\s/.test(after) ? " " : "") + after;
    input.focus();
    input.dispatchEvent(new Event("input", { bubbles: true }));
    field.querySelector(".options-box").hidden = true;
  }
});

// The episode page's tabs: the one asked for, or else the first with something in it.
function showTab(want) {
  const box = document.querySelector(".epage-more");
  if (!box) return;
  const tabs = [...box.querySelectorAll("[data-tab]")];
  const pick = tabs.find((t) => t.dataset.tab === want && !t.disabled) || tabs.find((t) => t.getAttribute("aria-selected") === "true" && !t.disabled) || tabs.find((t) => !t.disabled);
  tabs.forEach((t) => t.setAttribute("aria-selected", String(t === pick)));
  box.querySelectorAll("[data-panel]").forEach((x) => { x.hidden = !pick || x.dataset.panel !== pick.dataset.tab; });
}
document.addEventListener("click", (e) => {
  const t = e.target.closest("[data-tab]");
  if (t) showTab(t.dataset.tab);
});

async function loadChapters() {
  const ol = document.querySelector("[data-chapters]");
  if (!ol) return;
  let list = [];
  try { list = await (await fetch(`/episodes/${ol.dataset.chapters}/chapters`)).json(); } catch { /* shown as none */ }
  if (!ol.isConnected) return;
  ol.innerHTML = "";
  list.forEach((c) => {
    const li = document.createElement("li"), b = document.createElement("button"), at = document.createElement("span");
    b.type = "button";
    b.dataset.seek = c.start;
    at.className = "num";
    at.textContent = clock(c.start);
    b.append(at, c.title);
    li.append(b);
    ol.append(li);
  });
  document.querySelector('[data-tab="chapters"]').disabled = !list.length;
  showTab();
  player.sync();
}

// Timestamps and chapters seek this episode, starting it if something else is playing.
document.addEventListener("click", (e) => {
  const s = e.target.closest("[data-seek]");
  if (!s) return;
  e.preventDefault();
  const host = s.closest("[data-episode],[data-chapters]");
  const id = Number(host ? (host.dataset.episode || host.dataset.chapters) : 0);
  const t = Number(s.dataset.seek);
  if (player.ep && player.ep.id === id) player.seek(t, true);
  else if (id) player.start(id, new URLSearchParams(location.search).get("from") || "", t);
});

document.addEventListener("click", async (e) => {
  const b = e.target.closest("[data-copy-time]");
  if (!b) return;
  const id = Number(b.dataset.copyTime);
  const t = player.ep && player.ep.id === id ? Math.floor(player.time()) : 0;
  await navigator.clipboard.writeText(`${location.origin}/episodes/${id}${t ? "?t=" + t : ""}`);
  b.lastChild.textContent = "Copied";
});

document.addEventListener("click", (e) => {
  const b = e.target.closest("[data-play]");
  if (!b) return;
  e.preventDefault();
  player.start(Number(b.dataset.play), b.dataset.from || "", b.dataset.start ? Number(b.dataset.start) : null);
});

// Player controls live in the footer and on the episode page; the page's own start its episode until it is the one playing.
const idlePage = (el) => {
  const own = el.closest("[data-episode-page]");
  return own && own !== player.page() ? own : null;
};
document.addEventListener("click", (e) => {
  const b = e.target.closest("[data-player]");
  if (!b || b.matches("select, input")) return;
  const idle = idlePage(b);
  if (idle) { if (b.dataset.player === "toggle") player.startPage(idle, null); return; }
  if (player.ep) player.act(b.dataset.player);
});
document.addEventListener("input", (e) => {
  if (!e.target.matches('[data-player="seek"]')) return;
  const idle = idlePage(e.target), part = e.target.value / 1000;
  if (idle) { player.paint([idle], part * Number(idle.dataset.duration), Number(idle.dataset.duration)); return; }
  player.dragging = true;
  player.paint(player.scopes(), part * player.duration(), player.duration());
});
document.addEventListener("change", (e) => {
  const el = e.target.closest("[data-player]");
  if (!el) return;
  const idle = idlePage(el), act = el.dataset.player;
  if (act === "seek" && idle) player.startPage(idle, Math.floor(el.value / 1000 * Number(idle.dataset.duration)));
  else if (act === "seek") { player.dragging = false; player.seek(el.value / 1000 * player.duration()); }
  else if (act === "speed") player.setSpeed(Number(el.value));
  else if (act === "sleep") player.sleep(el.value);
});
// The video leaves the page before the page is swapped or kept for Back, and returns once the new one settles.
document.addEventListener("htmx:beforeHistorySave", () => player.dock());
document.addEventListener("htmx:beforeSwap", (e) => { if (e.detail.target && e.detail.target.id === "page") player.dock(); });
document.addEventListener("htmx:historyRestore", () => pageReady());
document.addEventListener("fullscreenchange", () => player.wake());
document.addEventListener("mousemove", () => { if (document.fullscreenElement) player.wake(); });

const player = {
  root: null, media: null, ep: null, from: "", offset: 0, base: "", saveTimer: null, sleepTimer: null, sleepFor: "", stopAtEnd: false,
  skipNext: false, ending: false, skipping: false, chapters: [], loading: false, speed: 100, idleTimer: null,
  init() {
    this.root = document.getElementById("player");
    if (!this.root) return false;
    if (this.media) return true;
    this.media = document.getElementById("media");
    const m = this.media;
    const busy = (on) => () => { this.loading = on; this.flags(); };
    m.addEventListener("timeupdate", () => this.tick());
    m.addEventListener("play", () => { this.flags(); this.saveTimer = setInterval(() => this.save(), 30000); });
    m.addEventListener("pause", () => { this.flags(); clearInterval(this.saveTimer); this.save(); });
    m.addEventListener("waiting", busy(true));
    ["playing", "canplay", "loadedmetadata"].forEach((ev) => m.addEventListener(ev, busy(false)));
    m.addEventListener("ended", () => this.finish());
    m.addEventListener("error", () => { this.loading = false; this.flags(); if (this.ep) this.showError("This episode couldn't be played here."); });
    m.addEventListener("loadedmetadata", () => {
      if (!this.ep.joined && this.pendingStart) { m.currentTime = this.pendingStart; }
      this.pendingStart = 0;
      m.playbackRate = this.speed / 100;
    });
    m.addEventListener("click", () => { if (m.closest(".stage")) this.toggle(); });
    document.addEventListener("keydown", (e) => {
      if (!this.ep || e.target.closest("input:not([type=range]), textarea, select, [contenteditable]")) return;
      if (e.ctrlKey || e.metaKey || e.altKey) return;
      if (e.code === "Space") { e.preventDefault(); this.toggle(); }
      if (e.code === "ArrowLeft") { e.preventDefault(); this.act("back"); }
      if (e.code === "ArrowRight") { e.preventDefault(); this.act("forward"); }
      if (e.code === "KeyF") this.act("fullscreen");
    });
    window.addEventListener("pagehide", () => this.save());
    if ("mediaSession" in navigator) {
      const ms = navigator.mediaSession;
      ms.setActionHandler("play", () => m.play());
      ms.setActionHandler("pause", () => m.pause());
      ms.setActionHandler("seekbackward", () => this.act("back"));
      ms.setActionHandler("seekforward", () => this.act("forward"));
      ms.setActionHandler("seekto", (d) => this.seek(d.seekTime));
      ms.setActionHandler("nexttrack", () => this.playNext());
    }
    return true;
  },
  act(act) {
    if (act === "toggle") this.toggle();
    if (act === "back") this.seek(this.time() - this.ep.skip_back);
    if (act === "forward") this.seek(this.time() + this.ep.skip_forward);
    if (act === "close") { document.querySelectorAll("details.menu[open]").forEach((d) => { d.open = false; }); this.close(); }
    if (act === "fullscreen") this.fullscreen();
    if (act === "next-now") this.playNext();
    if (act === "next-done") this.nextDone();
    if (act === "next-skip") { this.skipNext = true; this.all(".player-notice").forEach((n) => { n.hidden = true; }); }
    if (act === "prev-chapter" || act === "next-chapter") this.chapter(act === "next-chapter" ? 1 : -1);
  },
  // The episode page, when it shows what is playing.
  page() {
    const p = document.querySelector("[data-episode-page]");
    return p && this.ep && Number(p.dataset.episodePage) === this.ep.id ? p : null;
  },
  scopes() { return [this.root, this.page()].filter(Boolean); },
  all(sel) { return this.scopes().flatMap((s) => [...s.querySelectorAll(sel)]); },
  slot(name, text) { this.all(`[data-slot="${name}"]`).forEach((x) => { x.textContent = text; }); },
  flags() {
    this.scopes().forEach((s) => {
      s.classList.toggle("playing", !this.media.paused);
      s.classList.toggle("loading", this.loading);
    });
  },
  paint(scopes, t, d, thumb) {
    scopes.forEach((s) => s.querySelectorAll("[data-slot]").forEach((x) => {
      if (x.dataset.slot === "pos") x.textContent = clock(t);
      if (x.dataset.slot === "dur") x.textContent = clock(d);
      if (x.dataset.slot === "left") x.textContent = "-" + clock(d - t);
    }));
    if (thumb) scopes.forEach((s) => s.querySelectorAll('[data-player="seek"]').forEach((x) => { x.value = d ? Math.round(t / d * 1000) : 0; }));
  },
  dock() {
    if (!this.media) return;
    const dock = this.root.querySelector(".player-dock");
    if (this.media.parentElement !== dock) dock.append(this.media);
  },
  // Lines the footer and the episode page up with what is playing; the page is the player for its own episode.
  sync() {
    if (!this.init()) return;
    const el = document.querySelector("[data-episode-page]"), cur = this.page();
    this.root.hidden = !this.ep;
    this.root.classList.toggle("on-page", !!cur);
    document.documentElement.style.setProperty("--player", this.root.offsetHeight + "px");
    if (el) {
      el.classList.toggle("current", !!cur);
      el.querySelectorAll(".current-need").forEach((x) => { x.disabled = !cur; });
      if (!cur) {
        el.classList.remove("playing", "loading");
        el.querySelectorAll('[data-player="speed"]').forEach((x) => { x.value = el.dataset.speed === "0" ? "100" : el.dataset.speed; });
        el.querySelectorAll(".player-notice, .player-error, .next-offer").forEach((x) => { x.hidden = true; });
        this.paint([el], Number(el.dataset.position), Number(el.dataset.duration), true);
      }
    }
    const stage = cur && this.ep.media === "video" && cur.querySelector(".stage-pic");
    if (stage) { if (this.media.parentElement !== stage) stage.append(this.media); } else this.dock();
    if (!this.ep) return;
    this.flags();
    this.all('[data-player="speed"]').forEach((x) => { x.value = String(this.speed); });
    this.all('[data-player="sleep"]').forEach((x) => { x.value = this.sleepFor; });
    this.all('[data-player$="chapter"]').forEach((b) => { b.hidden = !this.chapters.length; });
    this.all(".player-notice").forEach((n) => { n.hidden = !this.noticed; });
    this.all(".next-offer").forEach((n) => { n.hidden = !this.offer; });
    this.slot("next", this.ep.next ? this.ep.next.title : "");
    this.all(".player-error").forEach((x) => {
      x.hidden = !this.error;
      const a = x.querySelector('[data-slot="page"]');
      a.hidden = !this.ep.page_url; a.href = this.ep.page_url || "#";
    });
    this.slot("error", this.error);
    this.tick();
  },
  startPage(el, at) {
    const t = at !== null ? at : (el.dataset.start ? Number(el.dataset.start) : null);
    return this.start(Number(el.dataset.episodePage), el.dataset.from || "", t);
  },
  async start(id, from, start, autoplay = true) {
    if (!this.init()) return;
    if (this.ep) this.save();
    const waiting = document.querySelector(`[data-episode-page="${id}"]`);
    if (waiting && autoplay) waiting.classList.add("loading");
    const q = new URLSearchParams({ from: from || "" });
    if (start !== null && start !== undefined) q.set("t", start);
    const res = await fetch(`/episodes/${id}/play?${q}`, { credentials: "same-origin" }).catch(() => null);
    if (!res || !res.ok) { if (waiting) waiting.classList.remove("loading"); return; }
    const ep = await res.json();
    this.ep = ep; this.from = from; this.skipNext = false; this.ending = false; this.noticed = false; this.offer = false;
    this.error = ep.error || ""; this.loading = autoplay; this.speed = ep.speed || 100; this.chapters = [];
    this.slot("title", ep.title); this.slot("channel", ep.channel);
    // The links get their address here, so htmx is told again to keep them from reloading the page and the player with it.
    this.root.querySelectorAll('[data-slot="link"]').forEach((a) => { a.href = `/episodes/${ep.id}${from ? "?from=" + encodeURIComponent(from) : ""}`; htmx.process(a); });
    const art = this.root.querySelector('[data-slot="art"]'); art.src = ep.artwork || "";
    this.root.classList.toggle("video", ep.media === "video");
    this.media.poster = ep.media === "video" ? ep.artwork || "" : "";
    this.base = ep.src;
    this.offset = 0;
    if (ep.joined) { this.offset = ep.start; this.media.src = ep.src + (ep.start ? "&t=" + ep.start : ""); }
    else { this.pendingStart = ep.start; this.media.src = ep.src; }
    this.sync();
    this.paint(this.scopes(), ep.start, ep.duration, true);
    if (ep.chapters) fetch(`/episodes/${ep.id}/chapters`).then((r) => r.json()).then((c) => {
      if (this.ep !== ep) return;
      this.chapters = c || [];
      this.sync();
    }).catch(() => {});
    if ("mediaSession" in navigator) {
      navigator.mediaSession.metadata = new MediaMetadata({ title: ep.title, artist: ep.channel, artwork: ep.artwork ? [{ src: ep.artwork, sizes: "512x512" }] : [] });
    }
    store.set("player", { id: ep.id, from });
    this.mark();
    if (autoplay) this.media.play().catch(() => { this.loading = false; this.flags(); });
  },
  showError(text) {
    this.error = text;
    this.sync();
  },
  restore() {
    const s = store.get("player");
    if (s && s.id) this.start(s.id, s.from, null, false);
  },
  mark() {
    document.querySelectorAll(".ep.playing").forEach((x) => x.classList.remove("playing"));
    if (this.ep) document.querySelectorAll(`.ep[data-id="${this.ep.id}"]`).forEach((x) => x.classList.add("playing"));
  },
  time() { return this.offset + (this.media ? this.media.currentTime : 0); },
  duration() {
    const d = this.media && isFinite(this.media.duration) ? this.media.duration + this.offset : 0;
    return this.ep && this.ep.duration ? this.ep.duration : d;
  },
  seek(t, play) {
    if (!this.ep) return;
    t = Math.max(0, Math.min(t, (this.duration() || t + 1) - 1));
    if (this.ep.joined) {
      this.offset = Math.floor(t);
      this.media.src = this.base + "&t=" + this.offset;
      this.media.play().catch(() => {});
    } else {
      this.media.currentTime = t;
      if (play) this.media.play().catch(() => {});
    }
  },
  toggle() { if (this.media.paused) this.media.play().catch(() => {}); else this.media.pause(); },
  setSpeed(v) {
    this.speed = v;
    this.media.playbackRate = v / 100;
    this.all('[data-player="speed"]').forEach((x) => { x.value = String(v); });
  },
  fullscreen() {
    const cur = this.page(), stage = cur && cur.querySelector(".stage");
    if (document.fullscreenElement) document.exitFullscreen();
    else if (stage && stage.requestFullscreen) stage.requestFullscreen().catch(() => {});
    else if (stage && this.media.webkitEnterFullscreen) this.media.webkitEnterFullscreen();
  },
  // Full screen hides its controls once the pointer rests.
  wake() {
    document.querySelectorAll(".stage.idle").forEach((x) => x.classList.remove("idle"));
    clearTimeout(this.idleTimer);
    const st = document.fullscreenElement;
    if (st) this.idleTimer = setTimeout(() => { if (!this.media.paused) st.classList.add("idle"); }, 3000);
  },
  tick() {
    if (!this.ep) return;
    const t = this.time(), d = this.duration();
    if (!this.dragging) this.paint(this.scopes(), t, d, true);
    const cur = this.page(), list = this.chapters;
    if (cur && list.length) {
      const at = list.findLastIndex((c) => c.start <= t);
      cur.querySelectorAll(".chapters li").forEach((li, i) => li.classList.toggle("on", i === at));
    }
    const end = d - (this.ep.skip_end || 0);
    if (d && this.ep.next && this.ep.autoplay && this.from.startsWith("playlist:") && !this.skipNext && !this.noticed && end - t < 30 && end - t > 0) {
      this.noticed = true;
      this.sync();
    }
    const offer = !!(d && this.ep.next && t >= d * (100 - this.ep.next_at) / 100);
    if (offer !== this.offer) { this.offer = offer; this.sync(); }
    if (d && this.ep.skip_end && t >= end && !this.ending) this.finish();
  },
  save() {
    if (!this.ep || !this.media || this.ending) return;
    const pos = Math.floor(this.time());
    if (pos < (this.ep.skip_start || 0) + 10) return;
    const fd = new FormData();
    fd.append("action", "position"); fd.append("position", pos);
    navigator.sendBeacon(`/episodes/${this.ep.id}/action`, fd);
  },
  async finish() {
    if (this.ending) return;
    this.ending = true;
    this.media.pause();
    await post(`/episodes/${this.ep.id}/action`, { action: "played" });
    if (this.stopAtEnd) { this.sleep(""); return; }
    if (this.ep.next && this.ep.autoplay && !this.skipNext) this.playNext();
  },
  // Next skips what is left: the episode counts as played and the next one starts.
  async nextDone() {
    if (!this.ep || !this.ep.next || this.skipping) return;
    this.skipping = true;
    try {
      // An episode that already ended is still left by Next, so only a second press is ignored.
      this.ending = true;
      this.media.pause();
      await post(`/episodes/${this.ep.id}/action`, { action: "played" });
      await this.playNext();
    } finally {
      this.skipping = false;
    }
  },
  // The episode page follows the player on to the next episode when it was showing the one that ended.
  async playNext() {
    if (!this.ep || !this.ep.next) return;
    if (!this.ending) this.save();
    const follow = this.page(), id = this.ep.next.id;
    await this.start(id, this.from, null);
    if (!follow || !follow.isConnected) return;
    const a = document.createElement("a");
    a.href = `/episodes/${id}${this.from ? "?from=" + encodeURIComponent(this.from) : ""}`;
    a.hidden = true;
    follow.append(a);
    htmx.process(a);
    a.click();
  },
  chapter(dir) {
    const t = this.time();
    const list = this.chapters;
    if (!list.length) return;
    let i = list.findIndex((c, k) => k === list.length - 1 || list[k + 1].start > t);
    if (dir < 0 && t - list[i].start < 3) i -= 1;
    if (dir > 0) i += 1;
    if (i >= 0 && i < list.length) this.seek(list[i].start, true);
    else if (dir < 0) this.seek(0, true);
  },
  sleep(v) {
    clearTimeout(this.sleepTimer);
    this.sleepFor = v;
    this.stopAtEnd = v === "end";
    if (v && v !== "end") this.sleepTimer = setTimeout(() => { this.media.pause(); this.sleep(""); }, Number(v) * 60000);
    this.all('[data-player="sleep"]').forEach((x) => { x.value = v; });
  },
  // Marking the playing episode played or unplayed stops it unsaved, so no later save puts it back in progress.
  marked(form, action) {
    if (!this.ep) return;
    const path = new URL(form.action, location.href).pathname;
    const one = path === `/episodes/${this.ep.id}/action` || !!form.querySelector(`input[name="id"][value="${this.ep.id}"]:checked`);
    const all = /\/mark(-played)?$/.test(path) && !!document.querySelector(`.ep[data-id="${this.ep.id}"]`);
    if (!all && !(one && (action === "played" || action === "unplayed"))) return;
    this.ending = true;
    this.close();
  },
  close() {
    this.save();
    if (document.fullscreenElement) document.exitFullscreen();
    this.media.pause();
    this.media.removeAttribute("src");
    this.media.load();
    this.sleep("");
    this.ep = null;
    this.loading = false;
    this.error = "";
    store.set("player", null);
    this.sync();
    this.mark();
  },
};

// Tag fields: lower case as you type, and existing tags offered on focus.
const tagBox = (input) => input.form && input.form.querySelector(".tag-suggest");
const showTags = (input) => {
  const box = tagBox(input);
  if (!box) return;
  const words = input.value.split(/[\s,]+/);
  const partial = /[\s,]$/.test(input.value) ? "" : words.pop().replace(/^#/, "");
  const have = new Set(words.map((w) => w.replace(/^#/, "")));
  let any = false;
  box.querySelectorAll("[data-tag]").forEach((b) => {
    b.hidden = have.has(b.dataset.tag) || !b.dataset.tag.includes(partial);
    any = any || !b.hidden;
  });
  box.hidden = !any;
};
document.addEventListener("focusin", (e) => { if (e.target.matches("[data-tag-input]")) showTags(e.target); });
document.addEventListener("focusout", (e) => {
  if (!e.target.matches("[data-tag-input]")) return;
  const box = tagBox(e.target);
  if (box) box.hidden = true;
});
document.addEventListener("input", (e) => {
  const el = e.target;
  if (!el.matches("[data-tag-input]")) return;
  const lower = el.value.toLowerCase();
  if (lower !== el.value) {
    const pos = el.selectionStart;
    el.value = lower;
    el.setSelectionRange(pos, pos);
  }
  showTags(el);
});
document.addEventListener("mousedown", (e) => { if (e.target.closest(".tag-suggest [data-tag]")) e.preventDefault(); });
document.addEventListener("click", (e) => {
  const b = e.target.closest(".tag-suggest [data-tag]");
  if (!b) return;
  const input = b.closest("form").querySelector("[data-tag-input]");
  const words = input.value.split(/[\s,]+/).filter(Boolean);
  if (!/[\s,]$/.test(input.value) && words.length) words.pop();
  input.value = [...words, b.dataset.tag].join(" ") + " ";
  input.focus();
  showTags(input);
});

// The Find box hides tiles whose name doesn't match, and keeps doing so as the grid refreshes.
function applyFind() {
  document.querySelectorAll("input[data-find]").forEach((input) => {
    const box = document.querySelector(input.dataset.find);
    if (!box) return;
    const q = input.value.trim().toLowerCase();
    let shown = 0;
    box.querySelectorAll("[data-name]").forEach((t) => {
      const hit = !q || t.dataset.name.toLowerCase().includes(q);
      t.hidden = !hit;
      if (hit) shown++;
    });
    const none = box.querySelector("[data-find-none]");
    if (none) none.hidden = shown > 0 || !q;
  });
}
document.addEventListener("input", (e) => { if (e.target.matches("input[data-find]")) applyFind(); });
document.addEventListener("htmx:afterSettle", applyFind);
