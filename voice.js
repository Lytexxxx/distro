"use strict";
/* ============ Vocal : salons vocaux + appels privés ============
 * Audio en WebRTC pair à pair (maillage, 8 personnes max par salon). Le serveur ne fait que relayer la
 * signalisation par le flux temps réel (SSE) et dire qui est dans quel salon. Chargé après app.js : utilise
 * ses globales (h, api, S, toast, fail, shown, userAv, memberOf, findUser, go, handlers, renderSide…).
 * Aucun innerHTML ni style en attribut (CSP stricte) : tout passe par le helper h().
 */

// Identifiant de l'onglet (survit à un rechargement, distinct d'un onglet à l'autre).
const TAB = (() => {
  const mk = () => Math.random().toString(36).slice(2, 12) + Date.now().toString(36);
  try { let t = sessionStorage.getItem("distro.tab"); if (!t) { t = mk(); sessionStorage.setItem("distro.tab", t); } return t; } catch { return mk(); }
})();

const V = {
  cid: null,            // salon vocal où je suis (serveur ou MP)
  stream: null,         // mon micro
  noMic: false,         // connecté sans micro (écoute seule)
  muted: false, deaf: false, wasMuted: false,
  ringing: false,       // j'appelle et ça sonne
  busy: false,
  joinedAt: 0, rejoins: 0, rejoinT: null,
  ice: [],
  peers: new Map(),     // userId -> {uid, pc, q, in, pending, audio}
  rooms: new Map(),     // salon -> [{id, muted, deaf}]
  rings: new Map(),     // appels entrants : salon de MP -> utilisateur qui appelle
  an: new Map(),        // userId -> analyseur (détection de parole)
  speaking: new Set(),
  ac: null,             // AudioContext
};

const audioBox = h("div", { class: "vaudio", "aria-hidden": "true" });
const ringBox = h("div", { id: "call-ring", "aria-live": "assertive" });
document.body.append(audioBox, ringBox);

/* ---------- Sons (générés, aucun fichier) ---------- */
function audioCtx() {
  if (!V.ac) { try { V.ac = new (window.AudioContext || window.webkitAudioContext)(); } catch { return null; } }
  if (V.ac.state === "suspended") V.ac.resume().catch(() => {});
  return V.ac;
}
// les navigateurs exigent un geste de l'utilisateur avant de laisser jouer un son
for (const ev of ["pointerdown", "keydown"]) document.addEventListener(ev, () => audioCtx(), { passive: true });

function tone(freq, start, dur, vol = 0.06) {
  const ac = audioCtx();
  if (!ac || ac.state !== "running") return;
  const t0 = ac.currentTime + start, o = ac.createOscillator(), g = ac.createGain();
  o.type = "sine"; o.frequency.value = freq;
  g.gain.setValueAtTime(0, t0);
  g.gain.linearRampToValueAtTime(vol, t0 + 0.02);
  g.gain.linearRampToValueAtTime(0, t0 + dur);
  o.connect(g); g.connect(ac.destination);
  o.start(t0); o.stop(t0 + dur + 0.05);
}
const sfx = {
  join() { tone(660, 0, 0.12); tone(880, 0.12, 0.16); },
  leave() { tone(880, 0, 0.12); tone(520, 0.12, 0.18); },
};
let ringTimer = null;
function startRing(kind) {
  stopRing();
  const play = kind === "in" ? () => { tone(880, 0, 0.22, 0.09); tone(660, 0.28, 0.22, 0.09); } : () => tone(440, 0, 0.9, 0.05);
  play();
  ringTimer = setInterval(play, kind === "in" ? 2000 : 3000);
}
function stopRing() { clearInterval(ringTimer); ringTimer = null; }

/* ---------- Détection de parole (anneau vert autour de l'avatar) ---------- */
function watch(id, stream) {
  unwatch(id);
  const ac = audioCtx();
  if (!ac || !stream.getAudioTracks().length) return;
  try {
    const src = ac.createMediaStreamSource(stream), an = ac.createAnalyser();
    an.fftSize = 512; src.connect(an);
    V.an.set(id, { src, an, buf: new Uint8Array(an.fftSize), hold: 0 });
  } catch {}
}
function unwatch(id) {
  const w = V.an.get(id);
  if (w) { try { w.src.disconnect(); } catch {} V.an.delete(id); }
  V.speaking.delete(id);
}
function paintSpeaking() {
  for (const el of document.querySelectorAll("[data-vuid]")) el.classList.toggle("speaking", V.speaking.has(el.dataset.vuid));
}
setInterval(() => {
  let changed = false;
  for (const [id, w] of V.an) {
    w.an.getByteTimeDomainData(w.buf);
    let sum = 0;
    for (const b of w.buf) { const x = (b - 128) / 128; sum += x * x; }
    const loud = Math.sqrt(sum / w.buf.length) > 0.04 && !(S.me && id === S.me.id && V.muted);
    w.hold = loud ? 3 : Math.max(0, w.hold - 1); // petit maintien : évite le clignotement
    const on = w.hold > 0;
    if (on !== V.speaking.has(id)) { on ? V.speaking.add(id) : V.speaking.delete(id); changed = true; }
  }
  if (changed) paintSpeaking();
}, 120);

/* ---------- Libellés ---------- */
function voiceName(cid) {
  for (const sv of S.servers.values()) { const c = sv.channels.find((x) => x.id === cid); if (c) return `${c.name} — ${sv.name}`; }
  const d = S.dms.find((x) => x.channel_id === cid);
  return d ? "Appel avec " + shown(d.user) : "Appel";
}
function micError(e) {
  const n = e && e.name;
  if (n === "NotAllowedError" || n === "SecurityError") return "Accès au micro refusé : autorise-le dans le navigateur. Tu peux seulement écouter.";
  if (n === "NotFoundError" || n === "OverconstrainedError") return "Aucun micro détecté. Tu peux seulement écouter.";
  return "Micro indisponible (" + (n || "erreur") + "). Tu peux seulement écouter.";
}
// salon vocal d'un serveur où se trouve cet utilisateur (ou null)
function voiceRoomOf(sv, uid) {
  for (const c of sv.channels) if (c.kind === "voice" && (V.rooms.get(c.id) || []).some((u) => u.id === uid)) return c.id;
  return null;
}

/* ---------- Connexions pair à pair ---------- */
function closePeer(uid) {
  const p = V.peers.get(uid);
  if (!p) return;
  V.peers.delete(uid);
  p.pc.onicecandidate = p.pc.ontrack = p.pc.onconnectionstatechange = null;
  try { p.pc.close(); } catch {}
  if (p.audio) { p.audio.srcObject = null; p.audio.remove(); }
  unwatch(uid);
}

function sendSignal(uid, kind, data) {
  const p = V.peers.get(uid), cid = V.cid;
  if (!p || !cid) return;
  // file d'attente par pair : l'ordre offre → candidats est garanti
  p.q = p.q.then(() => api(`/api/voice/${cid}/signal`, "POST", { to: uid, kind, data })).catch(() => {});
}

function newPC(uid, offerer) {
  closePeer(uid);
  const pc = new RTCPeerConnection({ iceServers: V.ice });
  const p = { uid, pc, q: Promise.resolve(), in: Promise.resolve(), pending: [], audio: null };
  V.peers.set(uid, p);
  if (V.stream) for (const t of V.stream.getAudioTracks()) pc.addTrack(t, V.stream);
  else if (offerer) pc.addTransceiver("audio", { direction: "recvonly" });
  pc.onicecandidate = (e) => { if (e.candidate) sendSignal(uid, "ice", e.candidate.toJSON()); };
  pc.ontrack = (e) => attachRemote(p, e.streams[0] || new MediaStream([e.track]));
  pc.onconnectionstatechange = () => {
    if (pc.connectionState === "connected") V.rejoins = 0;
    if (pc.connectionState === "failed" && V.peers.get(uid) === p) scheduleRejoin(1500);
    renderDock();
  };
  return p;
}

function attachRemote(p, stream) {
  if (!p.audio) {
    p.audio = h("audio", { autoplay: true, playsinline: true });
    p.audio.srcObject = stream;
    p.audio.muted = V.deaf;
    audioBox.append(p.audio);
    p.audio.play().catch(() => {});
  }
  watch(p.uid, stream);
}

// Le nouvel arrivant appelle tous les présents (jamais de collision d'offres).
async function callPeer(uid) {
  const p = newPC(uid, true);
  try {
    const offer = await p.pc.createOffer();
    await p.pc.setLocalDescription(offer);
    sendSignal(uid, "offer", { type: offer.type, sdp: offer.sdp });
  } catch (e) { console.error("offre", e); }
}

function onSignal(d) {
  if (d.channel_id !== V.cid) return;
  if (d.kind === "offer") {
    const p = newPC(d.from, false); // une offre = quelqu'un vient d'arriver : on repart d'une connexion neuve
    p.in = p.in.then(async () => {
      await p.pc.setRemoteDescription(d.data);
      const ans = await p.pc.createAnswer();
      await p.pc.setLocalDescription(ans);
      sendSignal(d.from, "answer", { type: ans.type, sdp: ans.sdp });
      for (const c of p.pending.splice(0)) await p.pc.addIceCandidate(c).catch(() => {});
    }).catch((e) => console.error("offre reçue", e));
    return;
  }
  const p = V.peers.get(d.from);
  if (!p) return;
  p.in = p.in.then(async () => {
    if (d.kind === "answer") {
      await p.pc.setRemoteDescription(d.data);
      for (const c of p.pending.splice(0)) await p.pc.addIceCandidate(c).catch(() => {});
    } else if (p.pc.remoteDescription) await p.pc.addIceCandidate(d.data).catch(() => {});
    else p.pending.push(d.data);
  }).catch((e) => console.error("signal", e));
}

/* ---------- Rejoindre / quitter ---------- */
function teardown() {
  V.cid = null; V.ringing = false; V.noMic = false;
  clearTimeout(V.rejoinT);
  stopRing();
  for (const id of [...V.peers.keys()]) closePeer(id);
  if (V.stream) { for (const t of V.stream.getTracks()) t.stop(); V.stream = null; }
  if (S.me) unwatch(S.me.id);
  renderDock(); renderSide(); renderHead();
}

async function voiceLeave() {
  if (!V.cid) return;
  teardown(); sfx.leave();
  await api("/api/voice/leave", "POST", { tab: TAB }).catch(() => {});
}

async function voiceJoin(cid) {
  if (V.busy || V.cid === cid) return;
  V.busy = true;
  try {
    if (V.cid) { teardown(); await api("/api/voice/leave", "POST", { tab: TAB }).catch(() => {}); }
    audioCtx(); // on est dans un clic : le navigateur autorise le son
    let stream = null, noMic = false;
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      noMic = true;
      toast("Le micro n'est disponible qu'en HTTPS (ou sur localhost). Tu peux seulement écouter.", true);
    } else {
      try { stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } }); }
      catch (e) { noMic = true; toast(micError(e), true); }
    }
    if (noMic) V.muted = true;
    if (stream) for (const t of stream.getAudioTracks()) t.enabled = !V.muted;
    let r;
    try { r = await api(`/api/voice/${cid}/join`, "POST", { muted: V.muted, deaf: V.deaf, tab: TAB }); }
    catch (e) { if (stream) for (const t of stream.getTracks()) t.stop(); throw e; }
    V.cid = cid; V.stream = stream; V.noMic = noMic; V.ice = r.ice || [];
    V.joinedAt = Date.now(); V.rejoins = 0; V.rooms.set(cid, r.users);
    if (stream) watch(S.me.id, stream);
    if (r.dm && !r.peers.length) {
      if (r.ringing) { V.ringing = true; startRing("out"); }
      else {
        const d = S.dms.find((x) => x.channel_id === cid);
        toast(`${d ? shown(d.user) : "Cette personne"} est hors ligne : l'appel ne peut pas sonner.`, true);
        await voiceLeave();
        return;
      }
    }
    sfx.join();
    for (const id of r.peers) callPeer(id);
    renderDock(); renderSide(); renderHead();
  } finally { V.busy = false; }
}

// Reconnexion à la même salle (connexion perdue, coupure du flux temps réel…).
function scheduleRejoin(ms) {
  clearTimeout(V.rejoinT);
  if (!V.cid) return;
  if (V.rejoins >= 3) { toast("Connexion vocale perdue", true); teardown(); return; }
  V.rejoinT = setTimeout(voiceRejoin, ms);
}
async function voiceRejoin() {
  const cid = V.cid;
  if (!cid || V.busy) return;
  V.busy = true; V.rejoins++;
  try {
    for (const id of [...V.peers.keys()]) closePeer(id);
    const r = await api(`/api/voice/${cid}/join`, "POST", { muted: V.muted, deaf: V.deaf, tab: TAB });
    if (V.cid !== cid) return;
    V.ice = r.ice || []; V.joinedAt = Date.now(); V.rooms.set(cid, r.users);
    for (const id of r.peers) callPeer(id);
  } catch (e) { teardown(); fail(e); }
  finally { V.busy = false; renderDock(); renderSide(); }
}

/* ---------- Micro / casque ---------- */
function applyState() {
  if (V.stream) for (const t of V.stream.getAudioTracks()) t.enabled = !V.muted;
  for (const p of V.peers.values()) if (p.audio) p.audio.muted = V.deaf;
  if (V.cid) api(`/api/voice/${V.cid}/state`, "POST", { muted: V.muted, deaf: V.deaf }).catch(() => {});
  renderDock();
}
function toggleMute() {
  if (V.noMic) return toast("Pas de micro : autorise-le dans le navigateur puis rejoins le salon.", true);
  if (V.deaf) { V.deaf = false; V.muted = false; } else V.muted = !V.muted;
  applyState();
}
function toggleDeaf() {
  V.deaf = !V.deaf;
  if (V.deaf) { V.wasMuted = V.muted; V.muted = true; } else V.muted = V.noMic || V.wasMuted;
  applyState();
}

/* ---------- Appels privés ---------- */
function renderRings() {
  ringBox.replaceChildren(...[...V.rings].map(([cid, u]) =>
    h("div", { class: "call-card", role: "alertdialog", "aria-label": "Appel de " + shown(u) },
      userAv(u, undefined, false),
      h("div", { class: "who" }, h("b", {}, shown(u)), h("small", {}, "Appel vocal entrant…")),
      h("button", { class: "btn vdecline", onclick: () => declineCall(cid) }, "Refuser"),
      h("button", { class: "btn vaccept", onclick: () => acceptCall(cid) }, "Accepter"))));
  document.title = V.rings.size ? "📞 Appel entrant — Distro" : "Distro";
}
function dropRing(cid) {
  const u = V.rings.get(cid);
  V.rings.delete(cid);
  renderRings();
  if (!V.rings.size) stopRing();
  return u;
}
async function acceptCall(cid) {
  dropRing(cid);
  if (S.dms.some((x) => x.channel_id === cid)) go("home", null, cid);
  try { await voiceJoin(cid); } catch (e) { fail(e); }
}
function declineCall(cid) {
  dropRing(cid);
  api(`/api/voice/${cid}/decline`, "POST").catch(() => {});
}

/* ---------- Interface ---------- */
function connectedAll() {
  for (const p of V.peers.values()) if (p.pc.connectionState !== "connected") return false;
  return true;
}

function renderDock() {
  const dock = $("#voice-dock");
  if (!dock) return;
  if (!V.cid) { dock.hidden = true; dock.replaceChildren(); return; }
  const ok = !V.ringing && connectedAll();
  const toggle = (on, icon, title, fn) =>
    h("button", { class: "icon-btn" + (on ? " on" : ""), title, "aria-label": title, "aria-pressed": on ? "true" : "false", onclick: fn }, icon);
  dock.hidden = false;
  dock.replaceChildren(
    h("div", { class: "vinfo" },
      h("b", { class: ok ? "ok" : "" }, "🔊 " + (V.ringing ? "Ça sonne…" : ok ? "Vocal connecté" : "Connexion…")),
      h("small", { title: voiceName(V.cid) }, voiceName(V.cid))),
    h("div", { class: "vbtns" },
      toggle(V.muted, V.muted ? "🔇" : "🎙️", V.muted ? "Réactiver le micro" : "Couper le micro", toggleMute),
      toggle(V.deaf, V.deaf ? "🙉" : "🎧", V.deaf ? "Réactiver le son" : "Couper le son", toggleDeaf),
      h("button", { class: "icon-btn vhang", title: "Quitter le vocal", "aria-label": "Quitter le vocal", onclick: () => voiceLeave() }, "📞")));
}

function vuserEl(sv, vu) {
  const m = (sv && memberOf(sv, vu.id)) || findUser(vu.id, { id: vu.id, username: "?", color: "#94a3b8", avatar: 0, roles: [] });
  return h("button", {
    class: "vuser" + (V.speaking.has(vu.id) ? " speaking" : ""), "data-vuid": vu.id, "data-pop": "",
    onclick: (e) => { if (sv && vu.id !== S.me.id && memberOf(sv, vu.id)) memberMenu(e.currentTarget, sv, m); },
  },
    userAv(m, undefined, true),
    h("span", { class: "name" }, shown(m)),
    vu.deaf ? h("span", { class: "vflag", title: "Son coupé" }, "🙉") : vu.muted ? h("span", { class: "vflag", title: "Micro coupé" }, "🔇") : null);
}

// Lignes de la barre latérale pour un salon vocal : le salon, puis les personnes connectées.
function voiceChannelRows(sv, c) {
  const users = V.rooms.get(c.id) || [], active = V.cid === c.id, mod = can(sv, "channels");
  const row = h("button", {
    class: "row voice" + (active ? " active" : ""), title: active ? "Tu es connecté à ce salon" : "Rejoindre le salon vocal",
    onclick: () => { if (!active) voiceJoin(c.id).catch(fail); },
  },
    h("span", { class: "hash" }, "🔊"), h("span", { class: "name" }, c.name),
    users.length ? h("small", { class: "vcount" }, users.length + "") : null,
    mod && h("span", { class: "icon-btn x", role: "button", title: "Modifier le salon", "aria-label": "Modifier le salon", onclick: (e) => { e.stopPropagation(); channelEditModal(sv, c); } }, "✎"),
    mod && h("span", { class: "icon-btn x", role: "button", title: "Supprimer le salon", "aria-label": "Supprimer le salon",
      onclick: (e) => { e.stopPropagation(); if (confirm(`Supprimer le salon vocal « ${c.name} » ?`)) api("/api/channels/" + c.id, "DELETE").catch(fail); } }, "×"));
  const out = [row];
  if (users.length) out.push(h("div", { class: "vusers" }, users.map((u) => vuserEl(sv, u))));
  return out;
}

// Boutons de l'en-tête : « Appeler » dans un MP, et une pastille pour raccrocher depuis n'importe où (utile sur mobile).
function voiceHeadButtons() {
  const c = curChannel(), inThis = !!(V.cid && c && V.cid === c.id), out = [];
  if (S.cur.kind === "home" && c) {
    out.push(h("button", {
      class: "btn call-btn" + (inThis ? " on" : ""), title: inThis ? "Raccrocher" : "Lancer un appel vocal",
      onclick: () => (inThis ? voiceLeave() : voiceJoin(c.id).catch(fail)),
    }, inThis ? "📞 Raccrocher" : "📞 Appeler"));
  }
  if (V.cid && !inThis) {
    out.push(h("button", { class: "btn call-btn pill", title: "Quitter le vocal", onclick: () => voiceLeave() }, "🔊 " + voiceName(V.cid).split(" — ")[0] + " ✕"));
  }
  return out.length ? h("div", { class: "head-actions" }, out) : null;
}

/* ---------- Synchronisation avec le serveur ---------- */
// Appelée à chaque (re)connexion du flux temps réel avec l'instantané des salons vocaux.
function voiceSync(snap) {
  V.rooms = new Map(Object.entries(snap || {}));
  if (V.busy || !S.me) return;
  const inRoom = (cid) => (V.rooms.get(cid) || []).some((u) => u.id === S.me.id);
  if (V.cid) {
    if (!inRoom(V.cid) && Date.now() - V.joinedAt > 3000) scheduleRejoin(0); // le serveur m'a oublié : je me réinscris
  } else if ([...V.rooms.keys()].some(inRoom)) {
    api("/api/voice/leave", "POST", { tab: TAB }).catch(() => {}); // vestige d'un rechargement de la page
  }
}

Object.assign(handlers, {
  "voice.state"(d) {
    const prev = V.rooms.get(d.channel_id) || [];
    if (d.users.length) V.rooms.set(d.channel_id, d.users); else V.rooms.delete(d.channel_id);
    if (d.channel_id === V.cid) {
      const ids = new Set(d.users.map((u) => u.id)), before = new Set(prev.map((u) => u.id));
      for (const id of [...V.peers.keys()]) if (!ids.has(id)) closePeer(id);
      if (d.users.some((u) => u.id !== S.me.id && !before.has(u.id))) sfx.join();
      else if (prev.some((u) => u.id !== S.me.id && !ids.has(u.id))) sfx.leave();
      if (V.ringing && d.users.length > 1) { V.ringing = false; stopRing(); }
      if (!ids.has(S.me.id) && Date.now() - V.joinedAt > 3000) scheduleRejoin(0);
    }
    renderSide(); renderDock();
    paintSpeaking();
  },
  "voice.signal": onSignal,
  "voice.end"(d) {
    if (d.channel_id !== V.cid || (d.tab && d.tab === TAB)) return;
    const msg = {
      declined: "Appel refusé", missed: "Pas de réponse", ended: "Appel terminé",
      kicked: "Tu as été déconnecté du salon vocal", deleted: "Ce salon vocal a été supprimé",
      replaced: "Tu t'es connecté au vocal depuis un autre onglet",
    }[d.reason];
    teardown(); sfx.leave();
    if (msg) toast(msg);
  },
  "call.ring"(d) {
    V.rings.set(d.channel_id, d.from);
    renderRings(); startRing("in");
  },
  "call.cancel"(d) {
    const u = dropRing(d.channel_id);
    if (u && d.reason === "missed") toast("Appel manqué de " + shown(u));
  },
});

// Quitter la page : on prévient le serveur (sinon il attend le délai de grâce).
addEventListener("pagehide", () => {
  if (V.cid && navigator.sendBeacon) navigator.sendBeacon("/api/voice/leave", new Blob([JSON.stringify({ tab: TAB })], { type: "application/json" }));
});
