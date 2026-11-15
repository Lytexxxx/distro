"use strict";

/* ============ Utilitaires ============ */
const $ = (s) => document.querySelector(s);

function h(tag, props, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k === "style") Object.assign(e.style, v);
    else if (v === true) e.setAttribute(k, "");
    else if (v !== false && v != null) e.setAttribute(k, v);
  }
  for (const k of kids.flat()) if (k != null && k !== false) e.append(k);
  return e;
}

async function api(path, method = "GET", body) {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
    credentials: "same-origin",
  });
  let data = null;
  try { data = await res.json(); } catch {}
  if (!res.ok) {
    if (res.status === 401 && S.me) { location.reload(); }
    throw new Error((data && data.error) || "Erreur " + res.status);
  }
  return data;
}

function toast(msg, bad) {
  const t = h("div", { class: "toast" + (bad ? " bad" : "") }, msg);
  $("#toasts").append(t);
  setTimeout(() => t.remove(), 3500);
}
const fail = (e) => toast(e.message || String(e), true);

const store = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch {} },
};

const tf = new Intl.DateTimeFormat("fr-FR", { hour: "2-digit", minute: "2-digit" });
const df = new Intl.DateTimeFormat("fr-FR", { weekday: "long", day: "numeric", month: "long", year: "numeric" });
const dayKey = (ts) => { const d = new Date(ts); return d.getFullYear() + "-" + d.getMonth() + "-" + d.getDate(); };

/* ============ État ============ */
const S = {
  me: null,
  servers: new Map(),
  dms: [],
  cur: { kind: "home", sid: null, cid: null },
  msgs: new Map(),   // channelId -> messages[]
  more: new Map(),   // channelId -> encore des anciens messages ?
  unread: new Map(), // channelId -> nombre
  typing: new Map(), // "cid:uid" -> {name, until}
  reply: null,
  es: null,
  drafts: new Map(), // channelId -> pièces jointes en attente [{file, preview}]
  upload: null,      // envoi en cours : {cid, abort}
  limits: { max_file: 25 << 20, max_files: 5 }, // remplacé par les valeurs du serveur
};

const EMOJIS = "😀 😂 🤣 😊 😍 😎 🤔 😅 😭 😡 🥳 😴 👍 👎 👏 🙏 💪 🔥 ✨ 🎉 ❤️ 💙 💔 💯 👀 🤝 🙌 😮 😢 😬 🤯 🫡 ✅ ❌ ⚡ 🚀 🎮 🎵 ☕ 🍕 🍻 🐱 🐶 🌙 ⭐ 💡 📌 ⚠️".split(" ");

const curServer = () => S.servers.get(S.cur.sid);
const curChannel = () => {
  if (S.cur.kind === "server") return curServer()?.channels.find((c) => c.id === S.cur.cid);
  const d = S.dms.find((d) => d.channel_id === S.cur.cid);
  return d && { id: d.channel_id, name: shown(d.user) };
};
const myRole = (sv) => sv?.members.find((m) => m.id === S.me.id)?.role;

/* ---- Rôles et permissions (le serveur reste l'autorité ; ici on ne fait que masquer/désactiver) ---- */
const PERMS = [
  ["channels", "Gérer les salons", "Créer et supprimer des salons"],
  ["messages", "Gérer les messages", "Supprimer les messages des autres"],
  ["kick", "Expulser des membres", "Retirer un membre du serveur"],
  ["roles", "Gérer les rôles", "Créer, modifier et attribuer des rôles plus bas dans la hiérarchie"],
  ["server", "Personnaliser le serveur", "Modifier le nom, l'icône, la couleur et la description"],
];
const ROLE_COLORS = ["#4f7cff", "#38bdf8", "#22d3ee", "#34d399", "#a3e635", "#facc15", "#fb923c", "#f87171", "#f472b6", "#a78bfa", "#94a3b8"];
const roleOf = (sv, id) => sv?.roles.find((r) => r.id === id);
const memberOf = (sv, uid) => sv?.members.find((m) => m.id === uid);
const isOwner = (sv) => myRole(sv) === "owner";
const can = (sv, perm) => !!sv && (isOwner(sv) || !!memberOf(sv, S.me.id)?.roles.some((id) => roleOf(sv, id)?.perms.includes(perm)));
// plus petit = plus haut placé ; propriétaire = -1 ; sans rôle = sous tous les rôles
const rankOf = (sv, uid) => {
  const m = memberOf(sv, uid);
  if (!m) return Infinity;
  if (m.role === "owner") return -1;
  let best = sv.roles.length;
  sv.roles.forEach((r, i) => { if (m.roles.includes(r.id) && i < best) best = i; });
  return best;
};
const canManageRole = (sv, idx) => isOwner(sv) || (can(sv, "roles") && rankOf(sv, S.me.id) < idx);
const canGrant = (sv, perms) => perms.every((p) => can(sv, p));
const nameColor = (sv, uid, fallback) => {
  const m = memberOf(sv, uid);
  return roleOf(sv, m?.roles[0])?.color || fallback;
};
const roleCount = (sv, rid) => sv.members.filter((m) => m.roles.includes(rid)).length;

/* ---- Profils, avatars, icônes ---- */
const shown = (u) => u.display || u.username;
const avatarUrl = (id, v) => (v ? `/api/users/${id}/avatar?v=${v}` : null);
const iconUrl = (sv) => (sv.icon ? `/api/servers/${sv.id}/icon?v=${sv.icon}` : null);
// couleur de texte lisible sur un fond #rrggbb
const readable = (hex) => {
  const n = parseInt(hex.slice(1), 16);
  const lum = (0.299 * (n >> 16) + 0.587 * ((n >> 8) & 255) + 0.114 * (n & 255)) / 255;
  return lum > 0.6 ? "#04060b" : "#ffffff";
};
const blobToDataUrl = (blob) => new Promise((ok, ko) => {
  const r = new FileReader();
  r.onload = () => ok(r.result); r.onerror = () => ko(new Error("Lecture de l'image impossible"));
  r.readAsDataURL(blob);
});
// recadre en carré et réduit à size×size (WebP, sinon JPEG) : l'image reste très légère
async function imageToBlob(file, size) {
  if (!file.type.startsWith("image/")) throw new Error("Ce fichier n'est pas une image");
  const bmp = await createImageBitmap(file).catch(() => null);
  if (!bmp) throw new Error("Image illisible");
  const c = document.createElement("canvas");
  c.width = c.height = size;
  const g = c.getContext("2d");
  const s = Math.min(bmp.width, bmp.height);
  g.drawImage(bmp, (bmp.width - s) / 2, (bmp.height - s) / 2, s, s, 0, 0, size, size);
  if (bmp.close) bmp.close();
  const enc = (type) => new Promise((ok) => c.toBlob(ok, type, 0.88));
  let blob = await enc("image/webp");
  if (!blob || blob.type !== "image/webp") {
    g.globalCompositeOperation = "destination-over"; g.fillStyle = "#111833"; g.fillRect(0, 0, size, size);
    blob = await enc("image/jpeg");
  }
  if (!blob) throw new Error("Conversion de l'image impossible");
  return blob;
}
async function upload(path, blob) {
  const res = await fetch(path, { method: "PUT", headers: { "Content-Type": blob.type }, body: blob, credentials: "same-origin" });
  let data = null;
  try { data = await res.json(); } catch {}
  if (!res.ok) throw new Error((data && data.error) || "Erreur " + res.status);
  return data;
}
/* ---- Pièces jointes : briques communes ---- */
function fmtSize(n) {
  if (n < 1024) return n + " o";
  if (n < 1048576) return (n / 1024).toFixed(n < 10240 ? 1 : 0).replace(".", ",") + " Ko";
  return (n / 1048576).toFixed(n < 10485760 ? 1 : 0).replace(".", ",") + " Mo";
}
const ICONS = [
  [/\.(zip|rar|7z|gz|tgz|bz2|xz|tar)$/i, "🗜️"], [/\.pdf$/i, "📕"], [/\.(docx?|odt|rtf|txt|md)$/i, "📄"],
  [/\.(xlsx?|ods|csv)$/i, "📊"], [/\.(pptx?|odp)$/i, "📽️"], [/\.(exe|msi|apk|dmg|bat|sh|jar)$/i, "⚙️"],
  [/\.(js|ts|py|json|html?|css|lua|luau|rbxl|rbxm|cs|cpp|c|java|go|rs)$/i, "🧩"],
  [/\.(mp3|m4a|aac|flac|wav|ogg|oga|opus)$/i, "🎵"], [/\.(mp4|webm|mov|mkv|avi|m4v)$/i, "🎞️"],
  [/\.(png|jpe?g|gif|webp|bmp|svg|ico)$/i, "🖼️"],
];
const fileIcon = (name) => (ICONS.find(([re]) => re.test(name)) || [, "📎"])[1];
const fileUrl = (f, dl) => "/api/files/" + f.id + (dl ? "?dl=1" : "");

function findUser(uid, fallback) {
  if (uid === S.me.id) return { ...S.me, online: true };
  for (const sv of S.servers.values()) { const m = memberOf(sv, uid); if (m) return m; }
  const d = S.dms.find((x) => x.user.id === uid);
  return d ? d.user : fallback;
}

/* ============ Rendu : briques ============ */
function avatar(name, color, online, small, img) {
  const initial = (name || "?").slice(0, 1).toUpperCase();
  const a = h("div", { class: "avatar" + (small ? " sm" : ""), style: { background: color } }, img ? null : initial);
  if (img) a.append(h("img", { src: img, alt: "", draggable: "false", onerror: (e) => e.target.replaceWith(initial) }));
  if (online !== undefined) a.append(h("span", { class: "dot" + (online ? " on" : "") }));
  return a;
}
const userAv = (u, online, small) => avatar(shown(u), u.color, online, small, avatarUrl(u.id, u.avatar));

function fmt(text) {
  const frag = document.createDocumentFragment();
  for (const p of text.split(/(```[\s\S]*?```)/g)) {
    if (p.length >= 6 && p.startsWith("```") && p.endsWith("```")) {
      frag.append(h("pre", {}, h("code", {}, p.slice(3, -3).replace(/^\n/, ""))));
    } else {
      const re = /(\*\*[^*\n]+\*\*|\*[^*\n]+\*|`[^`\n]+`|https?:\/\/[^\s<]+)/g;
      let last = 0, m;
      while ((m = re.exec(p))) {
        if (m.index > last) frag.append(p.slice(last, m.index));
        const s = m[0];
        if (s.startsWith("**")) frag.append(h("strong", {}, s.slice(2, -2)));
        else if (s[0] === "*") frag.append(h("em", {}, s.slice(1, -1)));
        else if (s[0] === "`") frag.append(h("code", {}, s.slice(1, -1)));
        else frag.append(h("a", { href: s, target: "_blank", rel: "noopener noreferrer nofollow" }, s));
        last = m.index + s.length;
      }
      if (last < p.length) frag.append(p.slice(last));
    }
  }
  return frag;
}

/* ============ Popover / modales ============ */
const pop = $("#pop");
function openPop(anchor, content, opts = {}) {
  pop.replaceChildren(content);
  pop.hidden = false;
  const r = anchor.getBoundingClientRect();
  const pw = pop.offsetWidth, ph = pop.offsetHeight;
  let x = opts.right ? r.right - pw : r.left;
  let y = r.bottom + 6;
  if (y + ph > innerHeight - 8) y = Math.max(8, r.top - ph - 6);
  x = Math.min(Math.max(8, x), innerWidth - pw - 8);
  pop.style.left = x + "px";
  pop.style.top = y + "px";
}
const closePop = () => { pop.hidden = true; pop.replaceChildren(); };
document.addEventListener("mousedown", (e) => { if (!pop.hidden && !pop.contains(e.target) && !e.target.closest("[data-pop]")) closePop(); });
document.addEventListener("keydown", (e) => { if (e.key === "Escape") { closePop(); closeModal(); } });

function menu(anchor, items, opts) {
  openPop(anchor, h("div", {}, items.filter(Boolean).map(([label, fn, danger]) =>
    h("button", { class: "item" + (danger ? " danger" : ""), onclick: () => { closePop(); fn(); } }, label))), opts);
}

function emojiPicker(anchor, onPick, opts) {
  openPop(anchor, h("div", { class: "emojis" }, EMOJIS.map((e) =>
    h("button", { type: "button", onclick: () => { closePop(); onPick(e); } }, e))), opts);
}

let liveModal = null; // modale qui se redessine quand son serveur change (rôles)
function closeModal() { liveModal = null; $("#modal-root").replaceChildren(); }
function modal(title, ...body) {
  const bg = h("div", { class: "modal-bg", onmousedown: (e) => { if (e.target === bg) closeModal(); } },
    h("div", { class: "modal", role: "dialog", "aria-modal": "true", "aria-label": title }, h("h2", {}, title), ...body));
  $("#modal-root").replaceChildren(bg);
  const f = bg.querySelector("input, textarea, button.primary");
  if (f) f.focus();
  return bg;
}

function formModal(title, desc, label, placeholder, submitLabel, onSubmit) {
  const input = h("input", { placeholder, maxlength: 50, required: true });
  const err = h("p", { class: "err" });
  const form = h("form", { style: { display: "grid", gap: "14px" }, onsubmit: async (e) => {
    e.preventDefault();
    try { await onSubmit(input.value.trim()); closeModal(); } catch (x) { err.textContent = x.message; }
  } },
    desc && h("p", {}, desc),
    h("label", {}, label, input), err,
    h("div", { class: "actions" },
      h("button", { type: "button", class: "btn", onclick: closeModal }, "Annuler"),
      h("button", { class: "btn primary", type: "submit" }, submitLabel)));
  modal(title, form);
}

/* ============ Rendu : colonnes ============ */
function unreadFor(sv) { return sv.channels.reduce((n, c) => n + (S.unread.get(c.id) || 0), 0); }

function renderRail() {
  const rail = $("#rail");
  const dmUnread = S.dms.reduce((n, d) => n + (S.unread.get(d.channel_id) || 0), 0);
  const home = h("button", { class: "rail-btn" + (S.cur.kind === "home" ? " active" : "") + (dmUnread ? " unread" : ""), title: "Messages privés", "aria-label": "Messages privés",
    onclick: () => go("home", null, S.cur.kind === "home" ? S.cur.cid : lastDM()) }, logo());
  if (dmUnread) home.append(h("span", { class: "badge" }, dmUnread));
  rail.replaceChildren(home, h("div", { class: "rail-sep" }));
  for (const sv of S.servers.values()) {
    const n = unreadFor(sv);
    const img = iconUrl(sv);
    const b = h("button", { class: "rail-btn" + (S.cur.sid === sv.id ? " active" : "") + (n ? " unread" : "") + (img ? " has-img" : sv.accent ? " tinted" : ""), title: sv.name, "aria-label": sv.name,
      onclick: () => go("server", sv.id, lastChan(sv)) }, img ? h("img", { src: img, alt: "", draggable: "false" }) : sv.name.slice(0, 2).toUpperCase());
    if (sv.accent && !img) { b.style.setProperty("--srv", sv.accent); b.style.setProperty("--srv-fg", readable(sv.accent)); }
    if (n) b.append(h("span", { class: "badge" }, n > 99 ? "99+" : n));
    rail.append(b);
  }
  rail.append(h("button", { class: "rail-btn add", title: "Créer ou rejoindre un serveur", "aria-label": "Ajouter un serveur", onclick: addServerModal }, "+"));
}

function logo() {
  const s = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  s.setAttribute("viewBox", "0 0 32 32"); s.setAttribute("width", "26"); s.setAttribute("height", "26");
  const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
  p.setAttribute("d", "M8 6h8a10 10 0 0 1 0 20H8z"); p.setAttribute("fill", "currentColor");
  s.append(p);
  return s;
}

const lastKey = (id) => "distro.last." + id;
const lastChan = (sv) => { const c = store.get(lastKey(sv.id)); return sv.channels.find((x) => x.id === c && x.kind !== "voice")?.id || sv.channels.find((x) => x.kind !== "voice")?.id; };
const lastDM = () => S.dms[0]?.channel_id || null;

function renderSide() {
  const head = $("#side-head"), list = $("#side-list");
  list.replaceChildren();
  head.style.boxShadow = "";
  if (S.cur.kind === "server") {
    const sv = curServer();
    const ic = iconUrl(sv);
    if (sv.accent) head.style.boxShadow = `inset 0 -2px 0 ${sv.accent}`;
    head.replaceChildren(h("button", { "data-pop": "", onclick: (e) => serverMenu(e.currentTarget, sv) },
      ic ? h("img", { class: "head-icon", src: ic, alt: "", draggable: "false" }) : null,
      h("span", { class: "t" }, sv.name), h("span", {}, "▾")));
    list.append(h("div", { class: "sec" }, "Salons",
      can(sv, "channels") && h("button", { class: "icon-btn", title: "Créer un salon", "aria-label": "Créer un salon", onclick: () => createChannelModal(sv) }, "+")));
    const texts = sv.channels.filter((c) => c.kind !== "voice"), voices = sv.channels.filter((c) => c.kind === "voice");
    for (const c of texts) {
      const n = S.unread.get(c.id) || 0;
      list.append(h("button", { class: "row" + (c.id === S.cur.cid ? " active" : "") + (n ? " has-unread" : ""), title: c.topic || null, onclick: () => go("server", sv.id, c.id) },
        h("span", { class: "hash" }, "#"), h("span", { class: "name" }, c.name),
        n ? h("span", { class: "badge sm" }, n > 99 ? "99+" : n) : null,
        can(sv, "channels") && h("span", { class: "icon-btn x", role: "button", title: "Modifier le salon", "aria-label": "Modifier le salon",
          onclick: (e) => { e.stopPropagation(); channelEditModal(sv, c); } }, "✎"),
        can(sv, "channels") && texts.length > 1 && h("span", { class: "icon-btn x", role: "button", title: "Supprimer le salon", "aria-label": "Supprimer le salon",
          onclick: (e) => { e.stopPropagation(); if (confirm(`Supprimer #${c.name} et tous ses messages ?`)) api("/api/channels/" + c.id, "DELETE").catch(fail); } }, "×")));
    }
    if (voices.length || can(sv, "channels")) {
      list.append(h("div", { class: "sec" }, "Salons vocaux",
        can(sv, "channels") && h("button", { class: "icon-btn", title: "Créer un salon vocal", "aria-label": "Créer un salon vocal", onclick: () => createChannelModal(sv, "voice") }, "+")));
      for (const c of voices) list.append(...voiceChannelRows(sv, c));
    }
  } else {
    head.replaceChildren(h("span", { class: "t" }, "Messages privés"));
    list.append(h("div", { class: "sec" }, "Conversations",
      h("button", { class: "icon-btn", title: "Nouveau message", "aria-label": "Nouveau message privé", onclick: newDMModal }, "+")));
    if (!S.dms.length) list.append(h("p", { class: "sec", style: { textTransform: "none", letterSpacing: 0, fontWeight: 400 } }, "Aucune conversation pour l'instant."));
    for (const d of S.dms) {
      const n = S.unread.get(d.channel_id) || 0;
      list.append(h("button", { class: "row" + (d.channel_id === S.cur.cid ? " active" : "") + (n ? " has-unread" : ""), onclick: () => go("home", null, d.channel_id) },
        userAv(d.user, d.user.online, true), h("span", { class: "name" }, shown(d.user)),
        n ? h("span", { class: "badge sm" }, n) : null));
    }
  }
}

function serverMenu(anchor, sv) {
  menu(anchor, [
    ["Inviter des personnes", () => inviteModal(sv)],
    can(sv, "channels") && ["Créer un salon", () => createChannelModal(sv)],
    can(sv, "server") && ["Personnaliser le serveur", () => serverSettingsModal(sv.id)],
    (can(sv, "roles") || sv.roles.length > 0) && ["Rôles", () => rolesModal(sv.id)],
    myRole(sv) === "owner"
      ? ["Supprimer le serveur", () => { if (confirm(`Supprimer définitivement « ${sv.name} » ?`)) api("/api/servers/" + sv.id, "DELETE").catch(fail); }, true]
      : ["Quitter le serveur", () => { if (confirm(`Quitter « ${sv.name} » ?`)) api("/api/servers/" + sv.id + "/leave", "POST").catch(fail); }, true],
  ]);
}

function renderMeBar() {
  const me = userAv(S.me, true, true);
  me.style.cursor = "pointer"; me.title = "Modifier mon profil"; me.addEventListener("click", profileModal);
  $("#me-bar").replaceChildren(
    me,
    h("div", { class: "info" }, h("b", {}, shown(S.me)), h("small", {}, S.me.status || "En ligne")),
    h("button", { class: "icon-btn", title: "Modifier mon profil", "aria-label": "Modifier mon profil", onclick: profileModal }, "⚙"),
    h("button", { class: "icon-btn", title: "Se déconnecter", "aria-label": "Se déconnecter", onclick: async () => { await api("/api/logout", "POST").catch(() => {}); location.reload(); } }, "⏻"));
}

function renderHead() {
  const c = curChannel(), head = $("#chat-head");
  const burger = h("button", { class: "icon-btn menu", "aria-label": "Menu", onclick: () => $("#app").classList.toggle("nav-open") }, "☰");
  if (!c) { head.replaceChildren(...[burger, h("span", {}, "Distro"), voiceHeadButtons()].filter(Boolean)); return; }
  head.replaceChildren(...[burger,
    S.cur.kind === "server" ? h("span", { class: "hash" }, "#") : null, h("span", {}, c.name),
    c.topic ? h("span", { class: "topic", title: c.topic }, c.topic) : null, voiceHeadButtons()].filter(Boolean));
}

function renderMembers() {
  const box = $("#members"), app = $("#app");
  const sv = curServer();
  const show = S.cur.kind === "server" && sv;
  app.classList.toggle("no-members", !show);
  if (!show) return;
  const on = sv.members.filter((m) => m.online), off = sv.members.filter((m) => !m.online);
  box.replaceChildren();
  for (const [title, arr] of [["En ligne", on], ["Hors ligne", off]]) {
    if (!arr.length) continue;
    box.append(h("div", { class: "sec" }, `${title} — ${arr.length}`));
    for (const m of arr) {
      box.append(h("button", { class: "row" + (m.online ? "" : " off"), "data-pop": "", title: [m.display && "@" + m.username, m.status].filter(Boolean).join(" · ") || null, onclick: (e) => memberMenu(e.currentTarget, sv, m) },
        userAv(m, m.online, true),
        h("span", { class: "name", style: { color: m.online ? nameColor(sv, m.id, m.color) : "" } }, shown(m)),
        m.role === "owner" ? h("span", { class: "role", title: "Propriétaire" }, "👑") : null,
        m.roles.length ? h("span", { class: "rdot", title: m.roles.map((id) => roleOf(sv, id)?.name).join(", "), style: { background: roleOf(sv, m.roles[0])?.color } }) : null));
    }
  }
}

function memberMenu(anchor, sv, m) {
  const me = S.me.id, isMe = m.id === me;
  // même règle que le serveur : on agit sur plus bas que soi (le propriétaire, sur tout le monde)
  const above = isOwner(sv) || (!isMe && rankOf(sv, me) < rankOf(sv, m.id));
  const items = [
    ["Voir le profil", () => viewProfile(m, sv)],
    !isMe && ["Envoyer un message", () => startDM(m.username)],
    sv.roles.length > 0 && above && (isOwner(sv) || can(sv, "roles")) && ["Gérer les rôles", () => memberRolesModal(sv.id, m.id)],
    can(sv, "kick") && !isMe && m.role !== "owner" && above && voiceRoomOf(sv, m.id) &&
      ["Déconnecter du vocal", () => api(`/api/voice/${voiceRoomOf(sv, m.id)}/kick/${m.id}`, "POST").catch(fail)],
    can(sv, "kick") && !isMe && m.role !== "owner" && above &&
      ["Expulser", () => { if (confirm(`Expulser ${m.username} ?`)) api(`/api/servers/${sv.id}/members/${m.id}`, "DELETE").catch(fail); }, true],
  ].filter(Boolean);
  if (!items.length) return;
  menu(anchor, items, { right: true });
}

/* ============ Rendu : messages ============ */
const msgsBox = $("#msgs");
const nearBottom = () => msgsBox.scrollHeight - msgsBox.scrollTop - msgsBox.clientHeight < 120;
const toBottom = () => { msgsBox.scrollTop = msgsBox.scrollHeight; };
// « collé en bas » : quand une image finit de charger, la liste grandit ; si on lisait le bas, on y reste
let pinned = true;

function lightbox(f) {
  const bg = h("div", { class: "modal-bg lightbox", role: "dialog", "aria-modal": "true", "aria-label": f.name, onmousedown: (e) => { if (e.target === bg || e.target.tagName === "FIGURE") closeModal(); } },
    h("figure", {},
      h("img", { src: fileUrl(f), alt: f.name }),
      h("figcaption", {}, h("span", {}, `${f.name} · ${fmtSize(f.size)}`),
        h("a", { class: "btn", href: fileUrl(f, true), download: f.name }, "Télécharger"),
        h("button", { type: "button", class: "btn primary", onclick: closeModal }, "Fermer"))));
  liveModal = null;
  $("#modal-root").replaceChildren(bg);
  bg.querySelector("button").focus();
}

function filesEl(m) {
  if (!m.files || !m.files.length) return null;
  const sizeLine = (f) => h("small", {}, fmtSize(f.size));
  return h("div", { class: "atts" }, m.files.map((f) => {
    const head = (extra) => h("div", { class: "top" }, h("span", { class: "ico" }, fileIcon(f.name)),
      h("span", { class: "meta2" }, h("b", { title: f.name }, f.name), sizeLine(f)), extra);
    const dl = h("a", { class: "dl", href: fileUrl(f, true), download: f.name, title: "Télécharger", "aria-label": "Télécharger " + f.name }, "⬇");
    switch (f.kind) {
      case "image":
        return h("button", { type: "button", class: "att-img", title: f.name, "aria-label": "Agrandir " + f.name, onclick: () => lightbox(f) },
          h("img", { src: fileUrl(f), alt: f.name, loading: "lazy", draggable: "false", onload: () => { if (pinned) toBottom(); } }));
      case "audio":
        return h("div", { class: "att att-audio" }, head(dl), h("audio", { controls: true, preload: "none", src: fileUrl(f) }));
      case "video":
        return h("div", { class: "att att-video" }, head(dl),
          h("video", { controls: true, preload: "metadata", playsinline: true, src: fileUrl(f), onloadedmetadata: () => { if (pinned) toBottom(); } }));
      default:
        return h("a", { class: "att att-file", href: fileUrl(f, true), download: f.name, title: "Télécharger " + f.name },
          h("span", { class: "ico" }, fileIcon(f.name)),
          h("span", { class: "meta2" }, h("b", {}, f.name), sizeLine(f)), h("span", { class: "dl" }, "⬇"));
    }
  }));
}

function nodesFor(list, i) {
  const m = list[i], prev = list[i - 1], out = [];
  if (!prev || dayKey(prev.ts) !== dayKey(m.ts)) out.push(h("div", { class: "day" }, df.format(m.ts)));
  const grouped = prev && dayKey(prev.ts) === dayKey(m.ts) && prev.author_id === m.author_id && !m.reply && m.ts - prev.ts < 5 * 60e3;
  out.push(msgEl(m, grouped));
  return out;
}

function msgAvatar(m, onclick) {
  const a = avatar(m.author_display || m.author, m.color, undefined, false, avatarUrl(m.author_id, m.avatar));
  a.style.cursor = "pointer"; a.addEventListener("click", onclick);
  return a;
}

function msgEl(m, grouped) {
  const sv = S.cur.kind === "server" ? curServer() : null;
  const mine = m.author_id === S.me.id;
  const openAuthor = () => viewProfile(findUser(m.author_id, { id: m.author_id, username: m.author, display: m.author_display, color: m.color, avatar: m.avatar, online: false, bio: "", status: "", banner: "" }), sv);
  const tools = h("div", { class: "tools" },
    h("button", { class: "icon-btn", title: "Réagir", "aria-label": "Réagir", "data-pop": "", onclick: (e) => emojiPicker(e.currentTarget, (em) => react(m, em, true), { right: true }) }, "☺"),
    h("button", { class: "icon-btn", title: "Répondre", "aria-label": "Répondre", onclick: () => setReply(m) }, "↩"),
    mine && m.content && h("button", { class: "icon-btn", title: "Modifier", "aria-label": "Modifier", onclick: () => editMsg(m) }, "✎"),
    (mine || (sv && can(sv, "messages"))) && h("button", { class: "icon-btn danger", title: "Supprimer", "aria-label": "Supprimer", onclick: () => { if (confirm("Supprimer ce message ?")) api("/api/messages/" + m.id, "DELETE").catch(fail); } }, "🗑"));
  const reactions = h("div", { class: "reactions" }, Object.entries(m.reactions || {}).map(([e, users]) =>
    h("button", { class: "chip" + (users.includes(S.me.id) ? " mine" : ""), onclick: () => react(m, e, !users.includes(S.me.id)) }, e, h("span", {}, users.length))));
  const text = m.content ? h("div", { class: "text" }, fmt(m.content)) : null;
  if (text && m.edited) text.append(" ", h("span", { class: "edited" }, "(modifié)"));
  return h("div", { class: "msg" + (grouped ? " grouped" : ""), "data-id": m.id },
    grouped ? h("div", { class: "gutter" }, tf.format(m.ts)) : h("div", { class: "gutter" }, msgAvatar(m, openAuthor)),
    h("div", { class: "body" },
      m.reply && h("div", { class: "reply-ref" }, h("b", {}, m.reply.author), h("span", {}, m.reply.content)),
      !grouped && h("div", { class: "meta" }, h("span", { class: "author", "data-uid": m.author_id, "data-base": m.color, style: { color: sv ? nameColor(sv, m.author_id, m.color) : m.color }, title: m.author_display ? "@" + m.author : null, onclick: openAuthor }, m.author_display || m.author), h("time", {}, tf.format(m.ts))),
      text, filesEl(m), reactions),
    tools);
}

function renderMessages(keepScroll) {
  const c = curChannel();
  const composer = $("#composer");
  if (!c) {
    composer.hidden = true;
    msgsBox.replaceChildren(h("div", { class: "empty" }, h("div", {},
      h("h2", {}, S.servers.size || S.dms.length ? "Choisis une conversation" : "Bienvenue sur Distro"),
      h("p", {}, S.servers.size || S.dms.length ? "Sélectionne un salon à gauche." : "Crée un serveur ou rejoins-en un avec un code d'invitation avec le bouton +."))));
    return;
  }
  composer.hidden = false;
  $("#input").placeholder = S.cur.kind === "server" ? "Écrire dans #" + c.name : "Écrire à " + c.name;
  const list = S.msgs.get(c.id);
  if (!list) { msgsBox.replaceChildren(); return; }
  const old = msgsBox.scrollHeight - msgsBox.scrollTop;
  const frag = document.createDocumentFragment();
  if (!S.more.get(c.id)) {
    frag.append(h("div", { class: "start" },
      h("h2", {}, S.cur.kind === "server" ? "Bienvenue dans #" + c.name : c.name),
      h("p", {}, S.cur.kind === "server" ? (c.topic || curServer()?.description || "C'est le début du salon.") : "C'est le début de votre conversation.")));
  }
  list.forEach((_, i) => frag.append(...nodesFor(list, i)));
  msgsBox.replaceChildren(frag);
  if (keepScroll === "top") msgsBox.scrollTop = msgsBox.scrollHeight - old; else toBottom();
}

function refreshAuthorColors() {
  const sv = S.cur.kind === "server" ? curServer() : null;
  if (!sv) return;
  for (const el of msgsBox.querySelectorAll(".author[data-uid]")) el.style.color = nameColor(sv, el.dataset.uid, el.dataset.base);
}

function replaceMsgEl(m) {
  const el = msgsBox.querySelector(`[data-id="${m.id}"]`);
  if (!el) return;
  el.replaceWith(msgEl(m, el.classList.contains("grouped")));
}

function renderTyping() {
  const now = Date.now(), names = [];
  for (const [k, v] of S.typing) {
    if (v.until < now) { S.typing.delete(k); continue; }
    if (k.startsWith(S.cur.cid + ":")) names.push(v.name);
  }
  $("#typing").textContent = !names.length ? "" : names.length === 1 ? `${names[0]} est en train d'écrire…` : `${names.slice(0, 3).join(", ")} écrivent…`;
}
setInterval(renderTyping, 1000);

function renderReply() {
  const bar = $("#reply-bar");
  bar.hidden = !S.reply;
  if (!S.reply) return;
  bar.replaceChildren(h("span", {}, "Réponse à ", h("b", {}, S.reply.author), " : " + S.reply.content.slice(0, 80)),
    h("button", { class: "icon-btn", "aria-label": "Annuler la réponse", onclick: () => { S.reply = null; renderReply(); } }, "×"));
}

function renderAll() {
  renderRail(); renderSide(); renderMeBar(); renderHead(); renderMembers(); renderReply(); renderTyping();
}

/* ============ Actions ============ */
async function loadChannel(cid) {
  if (!S.msgs.has(cid)) {
    try {
      const d = await api(`/api/channels/${cid}/messages`);
      S.msgs.set(cid, d.messages); S.more.set(cid, d.more);
    } catch (e) { return fail(e); }
  }
  if (S.cur.cid === cid) renderMessages();
}

function go(kind, sid, cid) {
  S.cur = { kind, sid, cid: cid || null };
  S.reply = null;
  if (cid) { S.unread.delete(cid); store.set("distro.cur", JSON.stringify(S.cur)); if (sid) store.set(lastKey(sid), cid); }
  $("#app").classList.remove("nav-open");
  renderAll(); renderMessages(); renderPending();
  if (cid) { loadChannel(cid); if (matchMedia("(pointer:fine)").matches) $("#input").focus(); }
}

async function loadOlder() {
  const cid = S.cur.cid, list = S.msgs.get(cid);
  if (!cid || !list || !list.length || !S.more.get(cid) || loadOlder.busy) return;
  loadOlder.busy = true;
  try {
    const d = await api(`/api/channels/${cid}/messages?before=${list[0].id}`);
    S.msgs.set(cid, [...d.messages, ...list]); S.more.set(cid, d.more);
    if (S.cur.cid === cid) renderMessages("top");
  } catch (e) { fail(e); } finally { loadOlder.busy = false; }
}
msgsBox.addEventListener("scroll", () => { pinned = nearBottom(); if (msgsBox.scrollTop < 40) loadOlder(); });

function react(m, emoji, on) { api(`/api/messages/${m.id}/react`, "POST", { emoji, on }).catch(fail); }

function setReply(m) {
  S.reply = { id: m.id, author: m.author_display || m.author, content: m.content || (m.files?.length ? "📎 " + m.files[0].name : "") };
  renderReply(); $("#input").focus();
}

function editMsg(m) {
  const el = msgsBox.querySelector(`[data-id="${m.id}"] .text`);
  if (!el) return;
  const ta = h("textarea", { rows: 2, maxlength: 4000 }, m.content);
  ta.value = m.content;
  const done = () => replaceMsgEl(m);
  ta.addEventListener("keydown", async (e) => {
    if (e.key === "Escape") return done();
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      const v = ta.value.trim();
      if (!v || v === m.content) return done();
      try { await api("/api/messages/" + m.id, "PATCH", { content: v }); } catch (x) { fail(x); done(); }
    }
  });
  el.replaceWith(h("div", { class: "edit-box" }, ta, h("small", {}, "Entrée pour enregistrer · Échap pour annuler")));
  ta.focus();
}

async function startDM(username) {
  try {
    const d = await api("/api/dms", "POST", { username });
    if (!S.dms.find((x) => x.channel_id === d.channel_id)) S.dms.push(d);
    go("home", null, d.channel_id);
  } catch (e) { fail(e); }
}

/* ============ Modales ============ */
function addServerModal() {
  const body = h("div", { style: { display: "grid", gap: "14px" } });
  const show = (tab) => {
    const create = tab === "create";
    const input = h("input", { placeholder: create ? "Mon super serveur" : "Code d'invitation (ex. K7M2QX9P)", maxlength: 60, required: true });
    const err = h("p", { class: "err" });
    body.replaceChildren(
      h("div", { class: "tabs" },
        h("button", { type: "button", class: create ? "on" : "", onclick: () => show("create") }, "Créer"),
        h("button", { type: "button", class: create ? "" : "on", onclick: () => show("join") }, "Rejoindre")),
      h("form", { style: { display: "grid", gap: "14px" }, onsubmit: async (e) => {
        e.preventDefault();
        try {
          const sv = await api(create ? "/api/servers" : "/api/servers/join", "POST", create ? { name: input.value } : { code: input.value });
          S.servers.set(sv.id, sv); closeModal(); go("server", sv.id, lastChan(sv));
        } catch (x) { err.textContent = x.message; }
      } },
        h("label", {}, create ? "Nom du serveur" : "Invitation", input), err,
        h("div", { class: "actions" }, h("button", { type: "button", class: "btn", onclick: closeModal }, "Annuler"),
          h("button", { class: "btn primary", type: "submit" }, create ? "Créer" : "Rejoindre"))));
    body.querySelector("input").focus();
  };
  modal("Ajouter un serveur", body);
  show("create");
}

function createChannelModal(sv, kind = "text") {
  let k = kind;
  const input = h("input", { maxlength: 50, required: true });
  const err = h("p", { class: "err" });
  const tabs = h("div", { class: "tabs" });
  const draw = () => {
    tabs.replaceChildren(
      h("button", { type: "button", class: k === "text" ? "on" : "", onclick: () => { k = "text"; draw(); } }, "# Textuel"),
      h("button", { type: "button", class: k === "voice" ? "on" : "", onclick: () => { k = "voice"; draw(); } }, "🔊 Vocal"));
    input.placeholder = k === "voice" ? "salon-vocal" : "nouveau-salon";
  };
  draw();
  const form = h("form", { style: { display: "grid", gap: "14px" }, onsubmit: async (e) => {
    e.preventDefault();
    try {
      const c = await api(`/api/servers/${sv.id}/channels`, "POST", { name: input.value.trim(), kind: k });
      closeModal();
      if (c.kind !== "voice") setTimeout(() => go("server", sv.id, c.id), 50);
    } catch (x) { err.textContent = x.message; }
  } },
    tabs,
    h("label", {}, "Nom du salon", input), err,
    h("div", { class: "actions" },
      h("button", { type: "button", class: "btn", onclick: closeModal }, "Annuler"),
      h("button", { class: "btn primary", type: "submit" }, "Créer")));
  modal("Créer un salon", form);
}

function newDMModal() {
  formModal("Nouveau message privé", "Entre le pseudo exact de la personne.", "Pseudo", "pseudo", "Ouvrir", (u) => startDM(u));
}

function inviteModal(sv) {
  const code = h("div", { class: "code" }, sv.invite);
  modal("Inviter sur " + sv.name, h("p", {}, "Partage ce code : il suffit de le coller dans « Rejoindre un serveur »."), code,
    h("div", { class: "actions" },
      h("button", { class: "btn", onclick: closeModal }, "Fermer"),
      h("button", { class: "btn primary", onclick: async () => {
        try { await navigator.clipboard.writeText(sv.invite); toast("Code copié"); } catch { toast("Sélectionne le code pour le copier", true); }
      } }, "Copier")));
}

/* ---- Rôles : gestion (liste + édition) ---- */
function rolesModal(sid, editing) {
  // editing : undefined = liste, "new" = création, sinon l'ID du rôle en cours d'édition
  const draw = () => {
    const sv = S.servers.get(sid);
    if (!sv) return closeModal();
    if (editing && editing !== "new" && !roleOf(sv, editing)) editing = undefined; // supprimé entre-temps
    const bg = editing ? roleEditor(sv, editing, (next) => { editing = next; draw(); }) : roleList(sv, (next) => { editing = next; draw(); });
    liveModal = (id) => { if (id === sid && !editing) draw(); };
    return bg;
  };
  draw();
}

function roleList(sv, go) {
  const mgr = can(sv, "roles");
  const move = (r, dir) => api(`/api/servers/${sv.id}/roles/${r.id}/move`, "POST", { dir }).catch(fail);
  const rows = sv.roles.map((r, i) => {
    const ok = canManageRole(sv, i);
    return h("div", { class: "role-row" },
      h("span", { class: "swatch", style: { background: r.color } }),
      h("div", { class: "grow" }, h("b", { style: { color: r.color } }, r.name),
        h("small", {}, `${roleCount(sv, r.id)} membre${roleCount(sv, r.id) > 1 ? "s" : ""} · ${r.perms.length} permission${r.perms.length > 1 ? "s" : ""}`)),
      ok && h("button", { class: "icon-btn", title: "Monter", "aria-label": "Monter " + r.name, disabled: i === 0 || !canManageRole(sv, i - 1), onclick: () => move(r, -1) }, "↑"),
      ok && h("button", { class: "icon-btn", title: "Descendre", "aria-label": "Descendre " + r.name, disabled: i === sv.roles.length - 1, onclick: () => move(r, 1) }, "↓"),
      ok ? h("button", { class: "icon-btn", title: "Modifier", "aria-label": "Modifier " + r.name, onclick: () => go(r.id) }, "✎")
         : h("span", { class: "icon-btn", title: "Au-dessus de ton rôle : lecture seule" }, "🔒"));
  });
  const bg = modal("Rôles — " + sv.name,
    h("p", {}, "Plus un rôle est haut dans la liste, plus il a d'autorité : on ne peut gérer que les rôles et les membres situés en dessous de son propre rôle. Le propriétaire a toujours tous les droits."),
    h("div", { class: "role-list" }, rows.length ? rows : h("p", {}, "Aucun rôle pour l'instant.")),
    h("div", { class: "actions" },
      h("button", { class: "btn", onclick: closeModal }, "Fermer"),
      mgr && h("button", { class: "btn primary", disabled: sv.roles.length >= 25, title: sv.roles.length >= 25 ? "Limite de 25 rôles atteinte" : "", onclick: () => go("new") }, "Créer un rôle")));
  bg.firstChild.classList.add("wide");
  return bg;
}

function roleEditor(sv, id, go) {
  const isNew = id === "new";
  const role = isNew ? { name: "", color: ROLE_COLORS[0], perms: [] } : roleOf(sv, id);
  let color = role.color;
  const name = h("input", { maxlength: 24, placeholder: "Modérateur", required: true });
  name.value = role.name;
  const err = h("p", { class: "err" });

  const swatches = h("div", { class: "swatches" });
  const custom = h("input", { type: "color", value: color, "aria-label": "Couleur personnalisée" });
  const paint = () => {
    swatches.replaceChildren(...ROLE_COLORS.map((c) => h("button", { type: "button", class: "sw" + (c === color ? " on" : ""), style: { background: c }, "aria-label": c, onclick: () => { color = c; custom.value = c; paint(); } })), custom);
  };
  custom.addEventListener("input", () => { color = custom.value; paint(); });
  paint();

  const boxes = PERMS.map(([key, label, desc]) => {
    const cb = h("input", { type: "checkbox" });
    cb.checked = role.perms.includes(key);
    // on peut toujours retirer une permission ; on n'en ajoute que si on la possède soi-même
    cb.disabled = !cb.checked && !can(sv, key);
    return [key, cb, h("label", { class: "perm" }, cb, h("span", {}, h("b", {}, label), h("small", {}, desc + (cb.disabled ? " — tu ne l'as pas toi-même" : ""))))];
  });

  const submit = async (e) => {
    e.preventDefault();
    const body = { name: name.value.trim(), color, perms: boxes.filter(([, cb]) => cb.checked).map(([k]) => k) };
    try {
      if (isNew) await api(`/api/servers/${sv.id}/roles`, "POST", body);
      else await api(`/api/servers/${sv.id}/roles/${id}`, "PATCH", body);
      go(undefined);
    } catch (x) { err.textContent = x.message; }
  };
  const remove = () => {
    if (!confirm(`Supprimer le rôle « ${role.name} » ? Il sera retiré de ses ${roleCount(sv, id)} membre(s).`)) return;
    api(`/api/servers/${sv.id}/roles/${id}`, "DELETE").then(() => go(undefined)).catch((x) => { err.textContent = x.message; });
  };
  const form = h("form", { style: { display: "grid", gap: "14px" }, onsubmit: submit },
    h("label", {}, "Nom du rôle", name),
    h("label", {}, "Couleur", swatches),
    h("div", { class: "perms" }, h("div", { class: "lbl" }, "Permissions"), boxes.map((b) => b[2])),
    err,
    h("div", { class: "actions" },
      !isNew && h("button", { type: "button", class: "btn danger", style: { marginRight: "auto" }, onclick: remove }, "Supprimer"),
      h("button", { type: "button", class: "btn", onclick: () => go(undefined) }, "Retour"),
      h("button", { class: "btn primary", type: "submit" }, isNew ? "Créer" : "Enregistrer")));
  const bg = modal(isNew ? "Nouveau rôle" : "Modifier le rôle", form);
  bg.firstChild.classList.add("wide");
  return bg;
}

/* ---- Rôles : attribution à un membre ---- */
function memberRolesModal(sid, uid) {
  const draw = () => {
    const sv = S.servers.get(sid), m = memberOf(sv, uid);
    if (!sv || !m) return closeModal();
    const me = S.me.id;
    const above = isOwner(sv) || (uid !== me && rankOf(sv, me) < rankOf(sv, uid));
    const rows = sv.roles.map((r, i) => {
      const has = m.roles.includes(r.id);
      let why = "";
      if (!above) why = "Ce membre est au même niveau que toi ou au-dessus";
      else if (!canManageRole(sv, i)) why = "Ce rôle est au-dessus du tien";
      else if (!isOwner(sv) && !has && !canGrant(sv, r.perms)) why = "Il contient des permissions que tu n'as pas";
      const cb = h("input", { type: "checkbox", disabled: !!why, onchange: async () => {
        cb.disabled = true;
        try { await api(`/api/servers/${sid}/members/${uid}/roles`, "POST", { role_id: r.id, on: cb.checked }); cb.disabled = false; }
        catch (x) { fail(x); draw(); } // on remet la case dans son état réel ; sinon "server.update" redessine
      } });
      cb.checked = has;
      return h("label", { class: "perm", title: why }, cb, h("span", {}, h("b", { style: { color: r.color } }, r.name), why && h("small", {}, why)));
    });
    const bg = modal("Rôles de " + m.username,
      h("div", { class: "role-list" }, rows.length ? rows : h("p", {}, "Ce serveur n'a pas encore de rôle.")),
      h("div", { class: "actions" }, h("button", { class: "btn primary", onclick: closeModal }, "Terminé")));
    liveModal = (id) => { if (id === sid) draw(); };
    return bg;
  };
  draw();
}

/* ---- Personnalisation : briques communes ---- */
function colorField(initial, allowNone) {
  let color = initial || "";
  const custom = h("input", { type: "color", value: color || "#4f7cff", "aria-label": "Couleur personnalisée" });
  const subs = [], btns = [];
  const mark = () => btns.forEach(([c, b]) => b.classList.toggle("on", c === color));
  const set = (c, fromPicker) => { color = c; if (c && !fromPicker) custom.value = c; mark(); subs.forEach((f) => f(color)); };
  if (allowNone) btns.push(["", h("button", { type: "button", class: "sw none", title: "Aucune", "aria-label": "Aucune couleur", onclick: () => set("") }, "∅")]);
  for (const c of ROLE_COLORS) btns.push([c, h("button", { type: "button", class: "sw", style: { background: c }, "aria-label": c, onclick: () => set(c) })]);
  custom.addEventListener("input", () => set(custom.value, true));
  mark();
  return { el: h("div", { class: "swatches" }, btns.map(([, b]) => b), custom), get: () => color, onChange: (f) => subs.push(f) };
}

// Choix d'une image : recadrée et réduite dans le navigateur, envoyée seulement à l'enregistrement.
function imageField(currentUrl, size) {
  const st = { blob: null, remove: false, preview: currentUrl };
  const subs = [];
  const err = h("small", { class: "err" });
  const file = h("input", { type: "file", accept: "image/png,image/jpeg,image/webp,image/gif", hidden: true });
  const rm = h("button", { type: "button", class: "btn", onclick: () => { st.blob = null; st.remove = true; st.preview = null; sync(); } }, "Retirer");
  const sync = () => { rm.hidden = !st.preview; subs.forEach((f) => f(st.preview)); };
  file.addEventListener("change", async () => {
    const f = file.files[0]; file.value = "";
    if (!f) return;
    err.textContent = "";
    try {
      const blob = await imageToBlob(f, size);
      st.preview = await blobToDataUrl(blob); st.blob = blob; st.remove = false; sync();
    } catch (x) { err.textContent = x.message; }
  });
  sync();
  return { el: h("div", { class: "imgfield" }, h("button", { type: "button", class: "btn", onclick: () => file.click() }, "Importer une image"), rm, file, err), st, onChange: (f) => subs.push(f) };
}

function profileCard(u, opts = {}) {
  const src = "img" in opts ? opts.img : avatarUrl(u.id, u.avatar);
  const av = avatar(shown(u), u.color, u.online, false, src);
  av.classList.add("xl");
  return h("div", { class: "pcard" },
    h("div", { class: "pbanner", style: { background: u.banner || u.color } }),
    h("div", { class: "pbody" }, av,
      h("div", { class: "pnames" }, h("b", {}, shown(u)), h("small", {}, "@" + u.username)),
      u.status ? h("div", { class: "pstatus" }, u.status) : null,
      u.bio ? h("div", {}, h("div", { class: "plabel" }, "À propos de moi"), h("p", { class: "pbio" }, u.bio)) : null,
      opts.roles && opts.roles.length ? h("div", {}, h("div", { class: "plabel" }, "Rôles"),
        h("div", { class: "rchips" }, opts.roles.map((r) => h("span", { class: "rchip" }, h("span", { class: "rdot", style: { background: r.color } }), r.name)))) : null));
}

function serverCard(s) {
  const ic = h("div", { class: "sicon" }, s.img ? h("img", { src: s.img, alt: "" }) : s.name.slice(0, 2).toUpperCase());
  if (!s.img && s.accent) { ic.style.background = s.accent; ic.style.color = readable(s.accent); }
  return h("div", { class: "pcard" },
    h("div", { class: "pbanner", style: { background: s.accent || "var(--bg4)" } }),
    h("div", { class: "pbody" }, ic,
      h("div", { class: "pnames" }, h("b", {}, s.name), h("small", {}, `${s.members} membre${s.members > 1 ? "s" : ""}`)),
      s.description ? h("p", { class: "pbio" }, s.description) : null));
}

/* ---- Profil ---- */
function viewProfile(u, sv) {
  const roles = sv ? (memberOf(sv, u.id)?.roles || []).map((id) => roleOf(sv, id)).filter(Boolean) : [];
  modal("Profil", profileCard(u, { roles }),
    h("div", { class: "actions" },
      u.id !== S.me.id && h("button", { class: "btn", onclick: () => { closeModal(); startDM(u.username); } }, "Envoyer un message"),
      u.id === S.me.id && h("button", { class: "btn", onclick: profileModal }, "Modifier mon profil"),
      h("button", { class: "btn primary", onclick: closeModal }, "Fermer")));
}

function profileModal() {
  const me = S.me;
  const display = h("input", { maxlength: 32, placeholder: me.username });
  const status = h("input", { maxlength: 64, placeholder: "Ex. : en train de coder" });
  const bio = h("textarea", { rows: 4, maxlength: 190, placeholder: "Parle un peu de toi…" });
  display.value = me.display || ""; status.value = me.status || ""; bio.value = me.bio || "";
  const color = colorField(me.color, false), banner = colorField(me.banner, true);
  const img = imageField(avatarUrl(me.id, me.avatar), 256);
  const prev = h("div");
  const redraw = () => prev.replaceChildren(profileCard({ ...me, display: display.value.trim(), status: status.value.trim(), bio: bio.value.trim(), color: color.get(), banner: banner.get(), online: true }, { img: img.st.preview }));
  for (const el of [display, status, bio]) el.addEventListener("input", redraw);
  color.onChange(redraw); banner.onChange(redraw); img.onChange(redraw);
  redraw();
  const err = h("p", { class: "err" });
  const form = h("form", { style: { display: "grid", gap: "14px" }, onsubmit: async (e) => {
    e.preventDefault();
    try {
      await api("/api/me", "PATCH", { display: display.value, status: status.value, bio: bio.value, color: color.get(), banner: banner.get() });
      if (img.st.blob) await upload("/api/me/avatar", img.st.blob);
      else if (img.st.remove && me.avatar) await api("/api/me/avatar", "DELETE");
      closeModal(); toast("Profil mis à jour");
    } catch (x) { err.textContent = x.message; }
  } },
    h("label", {}, "Nom affiché", display),
    h("label", {}, "Statut", status),
    h("label", {}, "À propos de moi", bio),
    h("div", {}, h("div", { class: "lbl" }, "Avatar"), img.el),
    h("div", {}, h("div", { class: "lbl" }, "Couleur du profil"), color.el),
    h("div", {}, h("div", { class: "lbl" }, "Bannière"), banner.el),
    h("p", {}, "Ton pseudo de connexion (@" + me.username + ") ne change pas : c'est lui que les autres utilisent pour t'écrire."),
    err,
    h("div", { class: "actions" },
      h("button", { type: "button", class: "btn", onclick: closeModal }, "Annuler"),
      h("button", { class: "btn primary", type: "submit" }, "Enregistrer")));
  const bg = modal("Mon profil", prev, form);
  bg.firstChild.classList.add("wide");
}

/* ---- Serveur ---- */
function serverSettingsModal(sid) {
  const sv = S.servers.get(sid);
  if (!sv) return;
  const name = h("input", { maxlength: 50, required: true });
  const desc = h("textarea", { rows: 3, maxlength: 190, placeholder: "À quoi sert ce serveur ?" });
  name.value = sv.name; desc.value = sv.description || "";
  const accent = colorField(sv.accent, true);
  const img = imageField(iconUrl(sv), 256);
  const prev = h("div");
  const redraw = () => prev.replaceChildren(serverCard({ name: name.value.trim() || sv.name, description: desc.value.trim(), accent: accent.get(), img: img.st.preview, members: sv.members.length }));
  for (const el of [name, desc]) el.addEventListener("input", redraw);
  accent.onChange(redraw); img.onChange(redraw);
  redraw();
  const err = h("p", { class: "err" });
  const form = h("form", { style: { display: "grid", gap: "14px" }, onsubmit: async (e) => {
    e.preventDefault();
    try {
      await api(`/api/servers/${sid}`, "PATCH", { name: name.value, description: desc.value, accent: accent.get() });
      if (img.st.blob) await upload(`/api/servers/${sid}/icon`, img.st.blob);
      else if (img.st.remove && sv.icon) await api(`/api/servers/${sid}/icon`, "DELETE");
      closeModal(); toast("Serveur mis à jour");
    } catch (x) { err.textContent = x.message; }
  } },
    h("label", {}, "Nom du serveur", name),
    h("label", {}, "Description", desc),
    h("div", {}, h("div", { class: "lbl" }, "Icône"), img.el),
    h("div", {}, h("div", { class: "lbl" }, "Couleur du serveur"), accent.el),
    err,
    h("div", { class: "actions" },
      h("button", { type: "button", class: "btn", onclick: closeModal }, "Annuler"),
      h("button", { class: "btn primary", type: "submit" }, "Enregistrer")));
  const bg = modal("Personnaliser " + sv.name, prev, form);
  bg.firstChild.classList.add("wide");
  liveModal = () => {}; // « server.remove » ferme la fenêtre ; les mises à jour ne la redessinent pas
}

function channelEditModal(sv, c) {
  const name = h("input", { maxlength: 32, required: true });
  const topic = h("input", { maxlength: 120, placeholder: "De quoi parle ce salon ?" });
  name.value = c.name; topic.value = c.topic || "";
  const err = h("p", { class: "err" });
  modal("Modifier #" + c.name, h("form", { style: { display: "grid", gap: "14px" }, onsubmit: async (e) => {
    e.preventDefault();
    try { await api(`/api/channels/${c.id}`, "PATCH", { name: name.value, topic: topic.value }); closeModal(); } catch (x) { err.textContent = x.message; }
  } },
    h("label", {}, "Nom du salon", name),
    h("label", {}, "Sujet", topic), err,
    h("div", { class: "actions" },
      h("button", { type: "button", class: "btn", onclick: closeModal }, "Annuler"),
      h("button", { class: "btn primary", type: "submit" }, "Enregistrer"))));
}

/* ============ Composer ============ */
const input = $("#input");
const autosize = () => { input.style.height = "auto"; input.style.height = Math.min(input.scrollHeight, innerHeight * 0.4) + "px"; };
input.addEventListener("input", () => {
  autosize();
  const now = Date.now();
  if (S.cur.cid && input.value && now - (input.lastTyping || 0) > 3000) {
    input.lastTyping = now;
    api(`/api/channels/${S.cur.cid}/typing`, "POST", {}).catch(() => {});
  }
});
input.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); $("#composer").requestSubmit(); }
});
$("#emoji-btn").dataset.pop = "";
$("#emoji-btn").addEventListener("click", (e) => emojiPicker(e.currentTarget, (em) => {
  const s = input.selectionStart ?? input.value.length;
  input.setRangeText(em, s, input.selectionEnd ?? s, "end"); input.focus(); autosize();
}));
/* ---- Pièces jointes : sélection, glisser-déposer, coller ---- */
const attachBar = $("#attach-bar");
const pending = (cid = S.cur.cid) => { let l = S.drafts.get(cid); if (!l) S.drafts.set(cid, (l = [])); return l; };
const isPreviewable = (f) => /^image\/(png|jpeg|gif|webp)$/.test(f.type); // aperçu local seulement : le serveur re-vérifie tout

function addPending(files) {
  const cid = S.cur.cid;
  if (!cid) return;
  if (S.upload) return toast("Un envoi est déjà en cours", true);
  const list = pending(cid), { max_file, max_files } = S.limits;
  for (const file of files) {
    if (list.length >= max_files) { toast(`${max_files} fichiers au maximum par message`, true); break; }
    if (file.size === 0) { toast(`« ${file.name} » est vide`, true); continue; }
    if (file.size > max_file) { toast(`« ${file.name} » dépasse ${fmtSize(max_file)}`, true); continue; }
    list.push({ file, preview: isPreviewable(file) ? URL.createObjectURL(file) : null });
  }
  renderPending();
}

function dropPending(cid, items) { // retire des fichiers de la liste d'un salon et libère leurs aperçus
  const list = pending(cid);
  for (const it of items) {
    const i = list.indexOf(it);
    if (i >= 0) list.splice(i, 1);
    if (it.preview) URL.revokeObjectURL(it.preview);
  }
}

function renderPending() {
  const list = S.cur.cid ? pending() : [];
  const busy = !!S.upload;
  attachBar.hidden = !list.length;
  $("#composer .send").disabled = busy;
  $("#attach-btn").disabled = busy;
  if (!list.length) { attachBar.replaceChildren(); return; }
  const chips = h("div", { class: "pends" }, list.map((it) =>
    h("div", { class: "pend" },
      h("div", { class: "thumb" }, it.preview ? h("img", { src: it.preview, alt: "" }) : fileIcon(it.file.name)),
      h("div", { class: "info" }, h("b", { title: it.file.name }, it.file.name), h("small", {}, fmtSize(it.file.size))),
      !busy && h("button", { type: "button", class: "icon-btn", title: "Retirer", "aria-label": "Retirer " + it.file.name, onclick: () => { dropPending(S.cur.cid, [it]); renderPending(); } }, "×"))));
  const bar = busy && S.upload.cid === S.cur.cid
    ? h("div", { class: "upbar" }, h("span", { class: "pct" }, "Envoi…"), h("div", { class: "track" }, h("i")),
        h("button", { type: "button", onclick: () => S.upload?.abort() }, "Annuler"))
    : null;
  attachBar.replaceChildren(...[chips, bar].filter(Boolean));
}
function setProgress(p) {
  const i = attachBar.querySelector(".track i"), t = attachBar.querySelector(".pct");
  if (i) i.style.width = Math.round(p * 100) + "%";
  if (t) t.textContent = p >= 1 ? "Traitement…" : `Envoi… ${Math.round(p * 100)} %`;
}

// XMLHttpRequest plutôt que fetch : seul lui donne la progression d'un envoi
function postForm(path, form, onProgress) {
  const x = new XMLHttpRequest();
  const promise = new Promise((ok, ko) => {
    x.open("POST", path);
    x.responseType = "json";
    x.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(e.loaded / e.total); };
    x.onload = () => {
      if (x.status === 401 && S.me) return location.reload();
      if (x.status >= 200 && x.status < 300) ok(x.response);
      else ko(new Error((x.response && x.response.error) || "Erreur " + x.status));
    };
    x.onerror = () => ko(new Error("Envoi impossible : connexion perdue ou fichier refusé par le serveur"));
    x.onabort = () => ko(new Error("Envoi annulé"));
    x.send(form);
  });
  return { promise, abort: () => x.abort() };
}

$("#attach-btn").addEventListener("click", () => $("#file-input").click());
$("#file-input").addEventListener("change", (e) => { addPending([...e.target.files]); e.target.value = ""; });
input.addEventListener("paste", (e) => { // capture d'écran / image copiée
  const files = [...(e.clipboardData?.files || [])];
  if (files.length) { e.preventDefault(); addPending(files); }
});
{
  const chat = $("#chat"), drop = $("#drop");
  let depth = 0;
  const hasFiles = (e) => [...(e.dataTransfer?.types || [])].includes("Files");
  chat.addEventListener("dragenter", (e) => { if (hasFiles(e) && S.cur.cid) { e.preventDefault(); depth++; drop.hidden = false; } });
  chat.addEventListener("dragover", (e) => { if (hasFiles(e) && S.cur.cid) e.preventDefault(); });
  chat.addEventListener("dragleave", () => { if (--depth <= 0) { depth = 0; drop.hidden = true; } });
  chat.addEventListener("drop", (e) => {
    depth = 0; drop.hidden = true;
    if (!hasFiles(e)) return;
    e.preventDefault();
    if (S.cur.cid) addPending([...e.dataTransfer.files]);
  });
  // un fichier lâché à côté ne doit jamais faire quitter l'application
  for (const t of ["dragover", "drop"]) window.addEventListener(t, (e) => { if (hasFiles(e)) e.preventDefault(); });
}

$("#composer").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (S.upload) return;
  const content = input.value.trim(), cid = S.cur.cid;
  const items = cid ? pending(cid).slice() : [];
  if ((!content && !items.length) || !cid) return;
  const reply = S.reply;
  input.value = ""; autosize(); S.reply = null; renderReply();
  try {
    let m;
    if (items.length) {
      const form = new FormData();
      form.append("content", content);
      if (reply?.id) form.append("reply_to", reply.id);
      for (const it of items) form.append("file", it.file, it.file.name);
      const up = postForm(`/api/channels/${cid}/messages`, form, setProgress);
      S.upload = { cid, abort: up.abort };
      renderPending(); setProgress(0);
      try { m = await up.promise; } finally { S.upload = null; }
      dropPending(cid, items);
    } else {
      m = await api(`/api/channels/${cid}/messages`, "POST", { content, reply_to: reply?.id });
    }
    renderPending();
    addMessage(m, true);
  } catch (x) {
    fail(x);
    if (!input.value) { input.value = content; autosize(); }
    if (reply && !S.reply && S.cur.cid === cid) { S.reply = reply; renderReply(); }
    renderPending();
  }
});

/* ============ Temps réel ============ */
function addMessage(m, mineJustSent) {
  const list = S.msgs.get(m.channel_id);
  if (list) {
    if (list.some((x) => x.id === m.id)) return;
    list.push(m);
  }
  S.typing.delete(m.channel_id + ":" + m.author_id);
  if (m.channel_id === S.cur.cid) {
    if (list) {
      const stick = nearBottom() || m.author_id === S.me.id;
      msgsBox.append(...nodesFor(list, list.length - 1));
      if (stick) toBottom();
    }
    renderTyping();
  } else if (m.author_id !== S.me.id) {
    S.unread.set(m.channel_id, (S.unread.get(m.channel_id) || 0) + 1);
    renderRail(); renderSide();
  }
}

const handlers = {
  "message.new": addMessage,
  "message.edit"(m) {
    const list = S.msgs.get(m.channel_id);
    const i = list ? list.findIndex((x) => x.id === m.id) : -1;
    if (i >= 0) { list[i] = m; if (m.channel_id === S.cur.cid) replaceMsgEl(m); }
  },
  "message.delete"(d) {
    const list = S.msgs.get(d.channel_id);
    if (!list) return;
    const i = list.findIndex((x) => x.id === d.id);
    if (i < 0) return;
    list.splice(i, 1);
    if (d.channel_id === S.cur.cid) { const keep = nearBottom(); renderMessages("top"); if (keep) toBottom(); }
  },
  reaction(d) {
    const m = S.msgs.get(d.channel_id)?.find((x) => x.id === d.message_id);
    if (m) { m.reactions = d.reactions; if (d.channel_id === S.cur.cid) replaceMsgEl(m); }
  },
  typing(d) {
    if (d.user_id === S.me.id) return;
    S.typing.set(d.channel_id + ":" + d.user_id, { name: d.name, until: Date.now() + 5000 });
    if (d.channel_id === S.cur.cid) renderTyping();
  },
  presence(d) {
    for (const sv of S.servers.values()) for (const m of sv.members) if (m.id === d.user_id) m.online = d.online;
    for (const x of S.dms) if (x.user.id === d.user_id) x.user.online = d.online;
    renderMembers(); renderSide();
  },
  "user.update"(u) {
    if (u.id === S.me.id) Object.assign(S.me, u);
    for (const sv of S.servers.values()) { const i = sv.members.findIndex((m) => m.id === u.id); if (i >= 0) sv.members[i] = { ...sv.members[i], ...u }; }
    for (const d of S.dms) if (d.user.id === u.id) d.user = { ...d.user, ...u };
    for (const list of S.msgs.values()) for (const m of list) if (m.author_id === u.id) { m.author = u.username; m.author_display = u.display; m.color = u.color; m.avatar = u.avatar; }
    renderAll();
    if (S.cur.cid) { const keep = nearBottom(); renderMessages("top"); if (keep) toBottom(); }
  },
  "server.update"(sv) {
    S.servers.set(sv.id, sv);
    if (S.cur.sid === sv.id && !sv.channels.some((c) => c.id === S.cur.cid)) return go("server", sv.id, lastChan(sv));
    renderAll();
    if (S.cur.sid === sv.id) refreshAuthorColors();
    liveModal?.(sv.id);
  },
  "server.remove"({ id }) {
    S.servers.delete(id);
    if (liveModal) closeModal();
    if (S.cur.sid === id) { toast("Tu n'as plus accès à ce serveur"); go("home", null, lastDM()); } else renderAll();
  },
  "dm.new"(d) {
    if (!S.dms.some((x) => x.channel_id === d.channel_id)) { S.dms.push(d); renderRail(); renderSide(); }
  },
};

function connect() {
  const es = new EventSource("/api/events");
  S.es = es;
  es.onopen = () => bootstrap().catch(fail);
  es.onmessage = (ev) => {
    try { const { t, d } = JSON.parse(ev.data); handlers[t]?.(d); } catch (e) { console.error(e); }
  };
}

async function bootstrap() {
  const d = await api("/api/me");
  const first = !S.me;
  S.me = d.user;
  if (d.limits) S.limits = d.limits;
  S.servers = new Map(d.servers.map((s) => [s.id, s]));
  S.dms = d.dms;
  voiceSync(d.voice);
  S.msgs.clear(); S.more.clear();
  if (first) {
    try { const c = JSON.parse(store.get("distro.cur") || "null"); if (c) S.cur = c; } catch {}
    if (S.cur.kind === "home" && !S.cur.cid && S.servers.size) { const sv = S.servers.values().next().value; S.cur = { kind: "server", sid: sv.id, cid: lastChan(sv) }; }
  }
  const c = S.cur;
  if (c.kind === "server") {
    const sv = S.servers.get(c.sid);
    if (!sv) S.cur = { kind: "home", sid: null, cid: null };
    else if (!sv.channels.some((x) => x.id === c.cid)) c.cid = lastChan(sv);
  } else if (c.cid && !S.dms.some((x) => x.channel_id === c.cid)) c.cid = null;
  $("#auth").hidden = true; $("#app").hidden = false;
  renderAll(); renderMessages();
  if (S.cur.cid) await loadChannel(S.cur.cid);
}

/* ============ Authentification ============ */
let registering = false;
function authMode(reg) {
  registering = reg;
  $("#auth-sub").textContent = reg ? "Crée ton compte en dix secondes." : "Content de te revoir.";
  $("#auth-submit").textContent = reg ? "Créer mon compte" : "Se connecter";
  $("#auth-switch-label").textContent = reg ? "Déjà un compte ?" : "Pas encore de compte ?";
  $("#auth-switch").textContent = reg ? "Se connecter" : "Créer un compte";
  $("#auth-pass").autocomplete = reg ? "new-password" : "current-password";
  $("#auth-err").textContent = "";
}
$("#auth-switch").addEventListener("click", (e) => { e.preventDefault(); authMode(!registering); });
$("#auth-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const btn = $("#auth-submit"); btn.disabled = true;
  try {
    await api(registering ? "/api/register" : "/api/login", "POST", { username: $("#auth-user").value, password: $("#auth-pass").value });
    connect();
  } catch (x) { $("#auth-err").textContent = x.message; } finally { btn.disabled = false; }
});
$("#app").addEventListener("click", (e) => { if (e.target.id === "app") $("#app").classList.remove("nav-open"); });

(async () => {
  // on attend que tous les scripts (dont voice.js) soient exécutés avant de démarrer
  if (document.readyState === "loading") await new Promise((ok) => document.addEventListener("DOMContentLoaded", ok, { once: true }));
  try { await api("/api/me"); connect(); } catch { $("#auth").hidden = false; $("#auth-user").focus(); }
})();
