package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Vocal : salons vocaux de serveur + appels privés (MP).
//
// L'audio ne passe PAS par le serveur : les navigateurs se parlent en WebRTC (maillage pair à pair).
// Le serveur ne fait que (1) savoir qui est dans quel salon, (2) relayer la signalisation (offre / réponse /
// candidats ICE) par le flux temps réel existant (SSE), (3) faire sonner l'appelé dans un appel privé.
// Tout est en mémoire : un redémarrage du serveur vide les salons vocaux (rien à sauvegarder).
//
// Une salle vocale a pour clé l'ID d'un salon : soit un salon « vocal » d'un serveur, soit un salon de MP
// (l'appel privé est donc simplement la salle vocale du MP).
//
// Ordre des verrous : Store.mu puis Voice.mu (jamais l'inverse). Les fonctions « voiceXxx » ci-dessous
// exigent que Store.mu soit déjà tenu (en lecture au moins) et ne le prennent jamais elles-mêmes.

const (
	KindText  = ""      // salon textuel (valeur historique : champ absent)
	KindVoice = "voice" // salon vocal

	maxVoicePeers = 8                // maillage : au-delà, la bande passante de chacun explose
	ringTimeout   = 45 * time.Second // durée de sonnerie d'un appel privé
	voiceGrace    = 10 * time.Second // coupure réseau tolérée avant de retirer quelqu'un du vocal
)

type vpeer struct {
	Muted bool
	Deaf  bool
	Tab   string // onglet du navigateur qui porte la connexion audio
	TS    int64
}

type ring struct {
	Caller string
	Callee string
	timer  *time.Timer
}

type Voice struct {
	mu    sync.Mutex
	rooms map[string]map[string]*vpeer // salon -> utilisateur -> état
	where map[string]string            // utilisateur -> salon vocal où il se trouve (un seul à la fois)
	rings map[string]*ring             // salon de MP -> sonnerie en cours
}

func NewVoice() *Voice {
	return &Voice{
		rooms: map[string]map[string]*vpeer{},
		where: map[string]string{},
		rings: map[string]*ring{},
	}
}

type VoiceUser struct {
	ID    string `json:"id"`
	Muted bool   `json:"muted"`
	Deaf  bool   `json:"deaf"`
}

// usersLocked : participants d'un salon, dans l'ordre d'arrivée. Voice.mu tenu.
func (v *Voice) usersLocked(cid string) []VoiceUser {
	room := v.rooms[cid]
	type entry struct {
		id string
		p  *vpeer
	}
	list := make([]entry, 0, len(room))
	for id, p := range room {
		list = append(list, entry{id, p})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].p.TS != list[j].p.TS {
			return list[i].p.TS < list[j].p.TS
		}
		return list[i].id < list[j].id
	})
	out := make([]VoiceUser, 0, len(list))
	for _, e := range list {
		out = append(out, VoiceUser{e.id, e.p.Muted, e.p.Deaf})
	}
	return out
}

func (v *Voice) users(cid string) []VoiceUser {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.usersLocked(cid)
}

// iceServers : STUN public par défaut ; un serveur TURN est ajouté si TURN_URL est défini
// (TURN_URL="turn:exemple.fr:3478,turns:exemple.fr:5349", TURN_USER, TURN_PASS).
// Sans TURN, deux personnes derrière des réseaux très restrictifs (certains 4G, entreprises) peuvent ne pas s'entendre.
func iceServers() []map[string]any {
	list := []map[string]any{{"urls": []string{"stun:stun.l.google.com:19302", "stun:stun1.l.google.com:19302"}}}
	if u := strings.TrimSpace(os.Getenv("TURN_URL")); u != "" {
		urls := []string{}
		for _, x := range strings.Split(u, ",") {
			if x = strings.TrimSpace(x); x != "" {
				urls = append(urls, x)
			}
		}
		list = append(list, map[string]any{"urls": urls, "username": os.Getenv("TURN_USER"), "credential": os.Getenv("TURN_PASS")})
	}
	return list
}

// ---------- Aides (Store.mu tenu) ----------

// voiceBroadcast diffuse la liste des participants d'un salon à tous ceux qui le voient.
func (a *App) voiceBroadcast(cid string) {
	c := a.s.Channels[cid]
	if c == nil {
		return
	}
	var sv *Server
	if len(c.DM) != 2 {
		if sv = a.s.Servers[c.ServerID]; sv == nil {
			return
		}
	}
	a.hub.send(a.audience(c, sv), "voice.state", map[string]any{"channel_id": cid, "users": a.voice.users(cid)})
}

// voiceSnapshot : état de tous les salons vocaux visibles par un utilisateur (envoyé dans /api/me).
func (a *App) voiceSnapshot(uid string) map[string][]VoiceUser {
	out := map[string][]VoiceUser{}
	v := a.voice
	v.mu.Lock()
	ids := make([]string, 0, len(v.rooms))
	for cid := range v.rooms {
		ids = append(ids, cid)
	}
	for _, cid := range ids {
		if _, _, ok := a.chanAccess(uid, cid); ok {
			out[cid] = v.usersLocked(cid)
		}
	}
	v.mu.Unlock()
	return out
}

// voiceLeave retire quelqu'un d'une salle (sans lui envoyer d'évènement) et gère les suites :
// appelant qui raccroche pendant la sonnerie, fin automatique d'un appel privé quand l'autre part.
func (a *App) voiceLeave(uid, cid string) {
	v := a.voice
	v.mu.Lock()
	room := v.rooms[cid]
	if room == nil || room[uid] == nil {
		v.mu.Unlock()
		return
	}
	delete(room, uid)
	if v.where[uid] == cid {
		delete(v.where, uid)
	}
	left := len(room)
	if left == 0 {
		delete(v.rooms, cid)
	}
	var cancelTo, lone string
	if rg := v.rings[cid]; rg != nil && rg.Caller == uid && left == 0 {
		rg.timer.Stop()
		delete(v.rings, cid)
		cancelTo = rg.Callee
	}
	if c := a.s.Channels[cid]; c != nil && len(c.DM) == 2 && left == 1 && v.rings[cid] == nil {
		for id := range room {
			lone = id
		}
	}
	v.mu.Unlock()

	if cancelTo != "" {
		a.hub.send([]string{cancelTo}, "call.cancel", map[string]string{"channel_id": cid, "reason": "cancelled"})
	}
	a.voiceBroadcast(cid)
	if lone != "" { // l'autre personne a raccroché : l'appel privé est terminé
		a.hub.send([]string{lone}, "voice.end", map[string]string{"channel_id": cid, "reason": "ended"})
		a.voiceLeave(lone, cid)
	}
}

// voiceEvict : retire quelqu'un de force et le prévient.
func (a *App) voiceEvict(uid, cid, reason string) {
	v := a.voice
	v.mu.Lock()
	in := v.rooms[cid] != nil && v.rooms[cid][uid] != nil
	v.mu.Unlock()
	if !in {
		return
	}
	a.hub.send([]string{uid}, "voice.end", map[string]string{"channel_id": cid, "reason": reason})
	a.voiceLeave(uid, cid)
}

// voiceEvictFromServer : à appeler quand quelqu'un perd l'accès à un serveur (expulsion, départ).
func (a *App) voiceEvictFromServer(sv *Server, uid string) {
	for _, cid := range sv.Channels {
		a.voiceEvict(uid, cid, "kicked")
	}
}

// voiceDropChannel : le salon est supprimé, tout le monde en sort.
func (a *App) voiceDropChannel(cid string) {
	v := a.voice
	v.mu.Lock()
	room := v.rooms[cid]
	ids := make([]string, 0, len(room))
	for id := range room {
		ids = append(ids, id)
		if v.where[id] == cid {
			delete(v.where, id)
		}
	}
	delete(v.rooms, cid)
	if rg := v.rings[cid]; rg != nil {
		rg.timer.Stop()
		delete(v.rings, cid)
	}
	v.mu.Unlock()
	if len(ids) > 0 {
		a.hub.send(ids, "voice.end", map[string]string{"channel_id": cid, "reason": "deleted"})
	}
}

// voiceDisconnect : la dernière connexion temps réel d'un utilisateur s'est fermée depuis un moment.
func (a *App) voiceDisconnect(uid string) {
	if a.hub.online(uid) {
		return // il est revenu pendant le délai de grâce
	}
	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	v := a.voice
	v.mu.Lock()
	cid := v.where[uid]
	v.mu.Unlock()
	if cid != "" {
		a.voiceLeave(uid, cid)
	}
}

// ringExpire : personne n'a décroché.
func (a *App) ringExpire(cid string, rg *ring) {
	v := a.voice
	v.mu.Lock()
	if v.rings[cid] != rg {
		v.mu.Unlock()
		return
	}
	delete(v.rings, cid)
	v.mu.Unlock()
	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	a.hub.send([]string{rg.Callee}, "call.cancel", map[string]string{"channel_id": cid, "reason": "missed"})
	a.hub.send([]string{rg.Caller}, "voice.end", map[string]string{"channel_id": cid, "reason": "missed"})
	a.voiceLeave(rg.Caller, cid)
}

// ---------- Routes ----------

// POST /api/voice/{id}/join  { muted, deaf, tab }
// Rejoint un salon vocal de serveur, ou démarre / rejoint l'appel d'un MP.
func (a *App) voiceJoin(w http.ResponseWriter, r *http.Request, u *User) {
	if !a.rl.Allow("vjoin:"+u.ID, 30, time.Minute) {
		fail(w, 429, "Trop de connexions au vocal, patiente un instant")
		return
	}
	var in struct {
		Muted bool   `json:"muted"`
		Deaf  bool   `json:"deaf"`
		Tab   string `json:"tab"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.Tab) > 40 {
		in.Tab = in.Tab[:40]
	}
	cid := r.PathValue("id")

	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	c, _, ok := a.chanAccess(u.ID, cid)
	if !ok {
		fail(w, 404, "Salon introuvable")
		return
	}
	isDM := len(c.DM) == 2
	if !isDM && c.Kind != KindVoice {
		fail(w, 400, "Ce n'est pas un salon vocal")
		return
	}
	canRing := !isDM || a.rl.Allow("ring:"+u.ID, 8, time.Minute)

	v := a.voice
	v.mu.Lock()
	prev := v.where[u.ID]
	v.mu.Unlock()
	if prev != "" && prev != cid { // un seul salon vocal à la fois
		a.hub.send([]string{u.ID}, "voice.end", map[string]string{"channel_id": prev, "reason": "replaced", "tab": in.Tab})
		a.voiceLeave(u.ID, prev)
	}

	v.mu.Lock()
	room := v.rooms[cid]
	if room == nil {
		room = map[string]*vpeer{}
		v.rooms[cid] = room
	}
	old := room[u.ID]
	if old == nil && len(room) >= maxVoicePeers {
		v.mu.Unlock()
		fail(w, 400, "Salon vocal plein (8 personnes maximum)")
		return
	}
	peers := make([]string, 0, len(room))
	for id := range room {
		if id != u.ID {
			peers = append(peers, id)
		}
	}
	ts := time.Now().UnixMilli()
	if old != nil {
		ts = old.TS
	}
	room[u.ID] = &vpeer{Muted: in.Muted, Deaf: in.Deaf, Tab: in.Tab, TS: ts}
	v.where[u.ID] = cid

	var callee string
	answered, ringStart := false, false
	if isDM {
		if rg := v.rings[cid]; rg != nil && rg.Callee == u.ID {
			rg.timer.Stop()
			delete(v.rings, cid)
			answered = true
		} else if len(peers) == 0 && v.rings[cid] == nil && canRing {
			callee = c.DM[0]
			if callee == u.ID {
				callee = c.DM[1]
			}
			rg := &ring{Caller: u.ID, Callee: callee}
			rg.timer = time.AfterFunc(ringTimeout, func() { a.ringExpire(cid, rg) })
			v.rings[cid] = rg
			ringStart = true
		}
	}
	v.mu.Unlock()

	if old != nil && old.Tab != in.Tab { // le même compte a rejoint depuis un autre onglet : l'ancien se déconnecte
		a.hub.send([]string{u.ID}, "voice.end", map[string]string{"channel_id": cid, "reason": "replaced", "tab": in.Tab})
	}
	if answered {
		a.hub.send([]string{u.ID}, "call.cancel", map[string]string{"channel_id": cid, "reason": "answered"})
	}
	ringing := false
	if ringStart {
		ringing = a.hub.online(callee)
		a.hub.send([]string{callee}, "call.ring", map[string]any{"channel_id": cid, "from": a.userView(u)})
	}
	a.voiceBroadcast(cid)
	writeJSON(w, map[string]any{"peers": peers, "ice": iceServers(), "users": v.users(cid), "ringing": ringing, "dm": isDM})
}

// POST /api/voice/leave  { tab }   — quitte le salon vocal courant (si l'onglet correspond).
func (a *App) voiceLeaveRoute(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Tab string `json:"tab"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	json.NewDecoder(r.Body).Decode(&in) // corps facultatif (sendBeacon)

	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	v := a.voice
	v.mu.Lock()
	cid := v.where[u.ID]
	match := cid != "" && (in.Tab == "" || (v.rooms[cid] != nil && v.rooms[cid][u.ID] != nil && v.rooms[cid][u.ID].Tab == in.Tab))
	v.mu.Unlock()
	if match {
		a.voiceLeave(u.ID, cid)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// POST /api/voice/{id}/state  { muted, deaf }
func (a *App) voiceSetState(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Muted bool `json:"muted"`
		Deaf  bool `json:"deaf"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !a.rl.Allow("vstate:"+u.ID, 60, time.Minute) {
		fail(w, 429, "Doucement !")
		return
	}
	cid := r.PathValue("id")
	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	v := a.voice
	v.mu.Lock()
	p := v.rooms[cid][u.ID]
	if p != nil {
		p.Muted, p.Deaf = in.Muted, in.Deaf
	}
	v.mu.Unlock()
	if p == nil {
		fail(w, 404, "Tu n'es pas dans ce salon vocal")
		return
	}
	a.voiceBroadcast(cid)
	writeJSON(w, map[string]bool{"ok": true})
}

// POST /api/voice/{id}/signal  { to, kind: offer|answer|ice, data }
func (a *App) voiceSignal(w http.ResponseWriter, r *http.Request, u *User) {
	if !a.rl.Allow("vsig:"+u.ID, 1200, time.Minute) {
		fail(w, 429, "Trop de signaux")
		return
	}
	var in struct {
		To   string          `json:"to"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if (in.Kind != "offer" && in.Kind != "answer" && in.Kind != "ice") || len(in.Data) == 0 || len(in.Data) > 32<<10 || in.To == u.ID {
		fail(w, 400, "Signal invalide")
		return
	}
	cid := r.PathValue("id")
	v := a.voice
	v.mu.Lock()
	room := v.rooms[cid]
	ok := room != nil && room[u.ID] != nil && room[in.To] != nil
	v.mu.Unlock()
	if !ok {
		fail(w, 404, "Destinataire absent du salon")
		return
	}
	a.hub.send([]string{in.To}, "voice.signal", map[string]any{"channel_id": cid, "from": u.ID, "kind": in.Kind, "data": in.Data})
	writeJSON(w, map[string]bool{"ok": true})
}

// POST /api/voice/{id}/decline — refuse un appel privé qui sonne.
func (a *App) voiceDecline(w http.ResponseWriter, r *http.Request, u *User) {
	cid := r.PathValue("id")
	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	v := a.voice
	v.mu.Lock()
	rg := v.rings[cid]
	if rg == nil || rg.Callee != u.ID {
		v.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	rg.timer.Stop()
	delete(v.rings, cid)
	v.mu.Unlock()
	a.hub.send([]string{u.ID}, "call.cancel", map[string]string{"channel_id": cid, "reason": "answered"}) // coupe la sonnerie sur ses autres onglets
	a.hub.send([]string{rg.Caller}, "voice.end", map[string]string{"channel_id": cid, "reason": "declined"})
	a.voiceLeave(rg.Caller, cid)
	writeJSON(w, map[string]bool{"ok": true})
}

// POST /api/voice/{id}/kick/{uid} — déconnecte quelqu'un d'un salon vocal (permission « expulser »).
func (a *App) voiceKick(w http.ResponseWriter, r *http.Request, u *User) {
	cid, uid := r.PathValue("id"), r.PathValue("uid")
	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	c, sv, ok := a.chanAccess(u.ID, cid)
	if !ok || sv == nil || c.Kind != KindVoice || uid == u.ID || sv.Members[uid] == "" ||
		!sv.has(u.ID, PermKick) || sv.Members[uid] == "owner" || !sv.outranks(u.ID, uid) {
		fail(w, 403, "Permission refusée")
		return
	}
	a.voiceEvict(uid, cid, "kicked")
	writeJSON(w, map[string]bool{"ok": true})
}
