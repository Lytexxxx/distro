package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	cookieName  = "distro_session"
	sessionTTL  = 30 * 24 * time.Hour
	maxMsgLen   = 4000
	pageSize    = 50
	maxChannels = 50
)

var palette = []string{"#4f7cff", "#6d8bff", "#38bdf8", "#22d3ee", "#34d399", "#a78bfa", "#f472b6", "#fb923c", "#facc15", "#94a3b8"}

type App struct {
	s          *Store
	hub        *Hub
	rl         *Limiter
	secure     bool // cookie Secure (derrière HTTPS)
	trustProxy bool
	maxFile    int64 // taille maximale d'une pièce jointe, en octets
	voice      *Voice
}

// ---------- Vues JSON ----------

type UserView struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Color    string `json:"color"`
	Online   bool   `json:"online"`
	Display  string `json:"display"`
	Bio      string `json:"bio"`
	Status   string `json:"status"`
	Banner   string `json:"banner"`
	Avatar   int64  `json:"avatar"` // version de l'image ; 0 = aucune (URL : /api/users/{id}/avatar?v=…)
}
type MemberView struct {
	UserView
	Role  string   `json:"role"`  // owner | member
	Roles []string `json:"roles"` // IDs des rôles personnalisés, du plus haut au plus bas
}
type ChannelView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Topic string `json:"topic"`
	Kind  string `json:"kind"` // "" = textuel, "voice" = vocal
}
type ServerView struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	OwnerID  string        `json:"owner_id"`
	Invite   string        `json:"invite"`
	Channels []ChannelView `json:"channels"`
	Members  []MemberView  `json:"members"`
	Roles    []RoleView    `json:"roles"` // du plus haut au plus bas

	Description string `json:"description"`
	Accent      string `json:"accent"`
	Icon        int64  `json:"icon"` // version de l'icône ; 0 = aucune (URL : /api/servers/{id}/icon?v=…)
}
type DMView struct {
	ChannelID string   `json:"channel_id"`
	User      UserView `json:"user"`
}
type ReplyView struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	Content string `json:"content"`
}
type MessageView struct {
	ID        string              `json:"id"`
	ChannelID string              `json:"channel_id"`
	AuthorID  string              `json:"author_id"`
	Author    string              `json:"author"`
	Color     string              `json:"color"`
	Display   string              `json:"author_display"`
	Avatar    int64               `json:"avatar"`
	Content   string              `json:"content"`
	TS        int64               `json:"ts"`
	Edited    bool                `json:"edited"`
	Reply     *ReplyView          `json:"reply,omitempty"`
	Reactions map[string][]string `json:"reactions"`
	Files     []FileView          `json:"files,omitempty"`
}

// Les fonctions de vue exigent que le verrou du store soit tenu (lecture au moins).

func (a *App) userView(u *User) UserView {
	return UserView{ID: u.ID, Username: u.Username, Color: u.Color, Online: a.hub.online(u.ID),
		Display: u.Display, Bio: u.Bio, Status: u.Status, Banner: u.Banner, Avatar: u.AvatarVer}
}

func (a *App) serverView(sv *Server) ServerView {
	v := ServerView{ID: sv.ID, Name: sv.Name, OwnerID: sv.OwnerID, Invite: sv.Invite,
		Description: sv.Description, Accent: sv.Accent, Icon: sv.IconVer,
		Channels: make([]ChannelView, 0, len(sv.Channels)), Members: make([]MemberView, 0, len(sv.Members)),
		Roles: make([]RoleView, 0, len(sv.Roles))}
	for _, r := range sv.Roles {
		v.Roles = append(v.Roles, roleView(r))
	}
	for _, cid := range sv.Channels {
		if c := a.s.Channels[cid]; c != nil {
			v.Channels = append(v.Channels, ChannelView{c.ID, c.Name, c.Topic, c.Kind})
		}
	}
	for uid, role := range sv.Members {
		if u := a.s.Users[uid]; u != nil {
			own := map[string]bool{}
			for _, rid := range sv.MemberRoles[uid] {
				own[rid] = true
			}
			ids := []string{}
			for _, r := range sv.Roles { // ordre de la hiérarchie
				if own[r.ID] {
					ids = append(ids, r.ID)
				}
			}
			v.Members = append(v.Members, MemberView{a.userView(u), role, ids})
		}
	}
	// tri : propriétaire, puis par rôle le plus haut, puis alphabétique
	sort.Slice(v.Members, func(i, j int) bool {
		x, y := v.Members[i], v.Members[j]
		if rx, ry := sv.rank(x.ID), sv.rank(y.ID); rx != ry {
			return rx < ry
		}
		return strings.ToLower(x.Username) < strings.ToLower(y.Username)
	})
	return v
}

func (a *App) messageView(m *Message) MessageView {
	v := MessageView{ID: m.ID, ChannelID: m.ChannelID, AuthorID: m.AuthorID, Content: m.Content,
		TS: m.TS, Edited: m.Edited, Reactions: m.Reactions, Author: "?", Color: palette[9], Files: fileViews(m.Files)}
	if v.Reactions == nil {
		v.Reactions = map[string][]string{}
	}
	if u := a.s.Users[m.AuthorID]; u != nil {
		v.Author, v.Color = u.Username, u.Color
		v.Display, v.Avatar = u.Display, u.AvatarVer
	}
	if m.ReplyTo != "" {
		if r := a.s.msgIdx[m.ReplyTo]; r != nil && !r.Deleted {
			name := "?"
			if u := a.s.Users[r.AuthorID]; u != nil {
				name = shownName(u)
			}
			c := r.Content
			if c == "" && len(r.Files) > 0 {
				c = "📎 " + r.Files[0].Name
			}
			if utf8.RuneCountInString(c) > 120 {
				c = string([]rune(c)[:120]) + "…"
			}
			v.Reply = &ReplyView{r.ID, name, c}
		}
	}
	return v
}

func (a *App) dmView(c *Channel, me string) DMView {
	other := c.DM[0]
	if other == me {
		other = c.DM[1]
	}
	v := DMView{ChannelID: c.ID}
	if u := a.s.Users[other]; u != nil {
		v.User = a.userView(u)
	}
	return v
}

// chanAccess vérifie qu'un utilisateur peut accéder à un salon.
func (a *App) chanAccess(uid, cid string) (*Channel, *Server, bool) {
	c := a.s.Channels[cid]
	if c == nil {
		return nil, nil, false
	}
	if len(c.DM) == 2 {
		return c, nil, c.DM[0] == uid || c.DM[1] == uid
	}
	sv := a.s.Servers[c.ServerID]
	if sv == nil {
		return nil, nil, false
	}
	_, ok := sv.Members[uid]
	return c, sv, ok
}

func (a *App) audience(c *Channel, sv *Server) []string {
	if sv == nil {
		return c.DM
	}
	out := make([]string, 0, len(sv.Members))
	for uid := range sv.Members {
		out = append(out, uid)
	}
	return out
}

func (a *App) members(sv *Server) []string {
	out := make([]string, 0, len(sv.Members))
	for uid := range sv.Members {
		out = append(out, uid)
	}
	return out
}

func (a *App) peers(uid string) []string {
	seen := map[string]struct{}{}
	for sid := range a.s.memberOf[uid] {
		if sv := a.s.Servers[sid]; sv != nil {
			for m := range sv.Members {
				seen[m] = struct{}{}
			}
		}
	}
	for _, cid := range a.s.userDMs[uid] {
		for _, p := range a.s.Channels[cid].DM {
			seen[p] = struct{}{}
		}
	}
	delete(seen, uid)
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}

// ---------- Utilitaires HTTP ----------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		fail(w, 400, "Requête invalide")
		return false
	}
	return true
}

func (a *App) clientIP(r *http.Request) string {
	if a.trustProxy {
		if x := r.Header.Get("X-Forwarded-For"); x != "" {
			return strings.TrimSpace(strings.Split(x, ",")[0])
		}
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func tokHash(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func hashPw(pw string, salt []byte) string {
	k, _ := pbkdf2.Key(sha256.New, pw, salt, 120000, 32)
	return hex.EncodeToString(k)
}

func (a *App) auth(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			fail(w, 401, "Non connecté")
			return
		}
		key := tokHash(c.Value)
		a.s.mu.RLock()
		sess, ok := a.s.Sessions[key]
		var u *User
		if ok && sess.Exp > time.Now().Unix() {
			u = a.s.Users[sess.UserID]
		}
		a.s.mu.RUnlock()
		if u == nil {
			fail(w, 401, "Session expirée")
			return
		}
		h(w, r, u)
	}
}

func validName(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < 2 || n > 24 {
		return false
	}
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func normChannel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if r := []rune(out); len(r) > 32 {
		out = string(r[:32])
	}
	return out
}

func colorFor(name string) string {
	h := 0
	for _, r := range name {
		h = (h*31 + int(r)) & 0x7fffffff
	}
	return palette[h%(len(palette)-1)]
}

func (a *App) setSession(w http.ResponseWriter, uid string) {
	token := randHex(32)
	a.s.Sessions[tokHash(token)] = Session{uid, time.Now().Add(sessionTTL).Unix()}
	a.s.touch()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		Secure: a.secure, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds())})
}

// ---------- Routes ----------

func (a *App) routes() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/register", a.register)
	m.HandleFunc("POST /api/login", a.login)
	m.HandleFunc("POST /api/logout", a.auth(a.logout))
	m.HandleFunc("GET /api/me", a.auth(a.me))
	m.HandleFunc("PATCH /api/me", a.auth(a.updateProfile))
	m.HandleFunc("PUT /api/me/avatar", a.auth(a.setAvatar))
	m.HandleFunc("DELETE /api/me/avatar", a.auth(a.clearAvatar))
	m.HandleFunc("GET /api/users/{id}/avatar", a.auth(a.userAvatar))
	m.HandleFunc("GET /api/events", a.auth(a.events))

	m.HandleFunc("POST /api/servers", a.auth(a.createServer))
	m.HandleFunc("POST /api/servers/join", a.auth(a.joinServer))
	m.HandleFunc("POST /api/servers/{id}/leave", a.auth(a.leaveServer))
	m.HandleFunc("DELETE /api/servers/{id}", a.auth(a.deleteServer))
	m.HandleFunc("PATCH /api/servers/{id}", a.auth(a.updateServer))
	m.HandleFunc("PUT /api/servers/{id}/icon", a.auth(a.setServerIcon))
	m.HandleFunc("DELETE /api/servers/{id}/icon", a.auth(a.clearServerIcon))
	m.HandleFunc("GET /api/servers/{id}/icon", a.auth(a.serverIcon))
	m.HandleFunc("POST /api/servers/{id}/channels", a.auth(a.createChannel))
	m.HandleFunc("DELETE /api/channels/{id}", a.auth(a.deleteChannel))
	m.HandleFunc("PATCH /api/channels/{id}", a.auth(a.updateChannel))
	m.HandleFunc("POST /api/servers/{id}/roles", a.auth(a.createRole))
	m.HandleFunc("PATCH /api/servers/{id}/roles/{rid}", a.auth(a.updateRole))
	m.HandleFunc("DELETE /api/servers/{id}/roles/{rid}", a.auth(a.deleteRole))
	m.HandleFunc("POST /api/servers/{id}/roles/{rid}/move", a.auth(a.moveRole))
	m.HandleFunc("POST /api/servers/{id}/members/{uid}/roles", a.auth(a.setMemberRole))
	m.HandleFunc("DELETE /api/servers/{id}/members/{uid}", a.auth(a.kick))

	m.HandleFunc("GET /api/channels/{id}/messages", a.auth(a.listMessages))
	m.HandleFunc("POST /api/channels/{id}/messages", a.auth(a.sendMessage))
	m.HandleFunc("POST /api/channels/{id}/typing", a.auth(a.typing))
	m.HandleFunc("GET /api/files/{id}", a.auth(a.serveAttachment))
	m.HandleFunc("PATCH /api/messages/{id}", a.auth(a.editMessage))
	m.HandleFunc("DELETE /api/messages/{id}", a.auth(a.deleteMessage))
	m.HandleFunc("POST /api/messages/{id}/react", a.auth(a.react))
	m.HandleFunc("POST /api/dms", a.auth(a.openDM))

	m.HandleFunc("POST /api/voice/leave", a.auth(a.voiceLeaveRoute))
	m.HandleFunc("POST /api/voice/{id}/join", a.auth(a.voiceJoin))
	m.HandleFunc("POST /api/voice/{id}/state", a.auth(a.voiceSetState))
	m.HandleFunc("POST /api/voice/{id}/signal", a.auth(a.voiceSignal))
	m.HandleFunc("POST /api/voice/{id}/decline", a.auth(a.voiceDecline))
	m.HandleFunc("POST /api/voice/{id}/kick/{uid}", a.auth(a.voiceKick))
	return m
}

// ---------- Comptes ----------

func (a *App) register(w http.ResponseWriter, r *http.Request) {
	if !a.rl.Allow("reg:"+a.clientIP(r), 5, time.Hour) {
		fail(w, 429, "Trop de créations de compte, réessaie plus tard")
		return
	}
	var in struct{ Username, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if !validName(in.Username) {
		fail(w, 400, "Pseudo : 2 à 24 caractères (lettres, chiffres, _ - .)")
		return
	}
	if len(in.Password) < 8 || len(in.Password) > 128 {
		fail(w, 400, "Mot de passe : 8 à 128 caractères")
		return
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	hash := hashPw(in.Password, salt)

	a.s.mu.Lock()
	key := strings.ToLower(in.Username)
	if _, taken := a.s.byName[key]; taken {
		a.s.mu.Unlock()
		fail(w, 409, "Ce pseudo est déjà pris")
		return
	}
	u := &User{ID: randHex(8), Username: in.Username, Hash: hash, Salt: hex.EncodeToString(salt),
		Color: colorFor(in.Username), Created: time.Now().Unix()}
	a.s.Users[u.ID] = u
	a.s.byName[key] = u.ID
	a.setSession(w, u.ID)
	v := a.userView(u)
	a.s.mu.Unlock()
	writeJSON(w, v)
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	ip := a.clientIP(r)
	if !a.rl.Allow("login:"+ip, 10, time.Minute) {
		fail(w, 429, "Trop de tentatives, patiente une minute")
		return
	}
	var in struct{ Username, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	a.s.mu.RLock()
	u := a.s.Users[a.s.byName[strings.ToLower(strings.TrimSpace(in.Username))]]
	a.s.mu.RUnlock()

	salt, _ := hex.DecodeString("00000000000000000000000000000000")
	want := ""
	if u != nil {
		salt, _ = hex.DecodeString(u.Salt)
		want = u.Hash
	}
	got := hashPw(in.Password, salt) // toujours calculé pour un temps de réponse constant
	if u == nil || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		fail(w, 401, "Pseudo ou mot de passe incorrect")
		return
	}
	a.s.mu.Lock()
	a.setSession(w, u.ID)
	v := a.userView(u)
	a.s.mu.Unlock()
	writeJSON(w, v)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request, u *User) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.s.mu.Lock()
		delete(a.s.Sessions, tokHash(c.Value))
		a.s.touch()
		a.s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) me(w http.ResponseWriter, r *http.Request, u *User) {
	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	servers := make([]ServerView, 0, len(a.s.memberOf[u.ID]))
	for sid := range a.s.memberOf[u.ID] {
		if sv := a.s.Servers[sid]; sv != nil {
			servers = append(servers, a.serverView(sv))
		}
	}
	sort.Slice(servers, func(i, j int) bool {
		return a.s.Servers[servers[i].ID].Created < a.s.Servers[servers[j].ID].Created
	})
	dms := make([]DMView, 0, len(a.s.userDMs[u.ID]))
	for _, cid := range a.s.userDMs[u.ID] {
		dms = append(dms, a.dmView(a.s.Channels[cid], u.ID))
	}
	writeJSON(w, map[string]any{"user": a.userView(u), "servers": servers, "dms": dms,
		"limits": map[string]int64{"max_file": a.maxFile, "max_files": maxFilesPerMsg},
		"voice": a.voiceSnapshot(u.ID)})
}

// ---------- Temps réel (Server-Sent Events) ----------

func (a *App) events(w http.ResponseWriter, r *http.Request, u *User) {
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "Streaming non supporté")
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	ch, first := a.hub.add(u.ID)
	if first {
		a.broadcastPresence(u.ID, true)
	}
	defer func() {
		a.hub.remove(u.ID, ch)
		if !a.hub.online(u.ID) {
			a.broadcastPresence(u.ID, false)
			// coupure brève (changement de réseau, veille) : on laisse le temps de se reconnecter avant de quitter le vocal
			time.AfterFunc(voiceGrace, func() { a.voiceDisconnect(u.ID) })
		}
	}()

	fmt.Fprint(w, "retry: 2000\n\n")
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (a *App) broadcastPresence(uid string, online bool) {
	a.s.mu.RLock()
	to := a.peers(uid)
	a.s.mu.RUnlock()
	a.hub.send(to, "presence", map[string]any{"user_id": uid, "online": online})
}

// ---------- Serveurs ----------

func inviteCode() string {
	const al = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = al[int(b[i])%len(al)]
	}
	return string(b)
}

func (a *App) pushServer(sv *Server) {
	a.hub.send(a.members(sv), "server.update", a.serverView(sv))
}

func (a *App) createServer(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(name); n < 2 || n > 50 {
		fail(w, 400, "Nom du serveur : 2 à 50 caractères")
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	if len(a.s.memberOf[u.ID]) >= 100 {
		fail(w, 400, "Limite de 100 serveurs atteinte")
		return
	}
	sv := &Server{ID: randHex(8), Name: name, OwnerID: u.ID, Members: map[string]string{},
		Roles: []*Role{}, MemberRoles: map[string][]string{}, Created: time.Now().Unix()}
	for {
		sv.Invite = inviteCode()
		if _, dup := a.s.invIdx[sv.Invite]; !dup {
			break
		}
	}
	for _, cn := range []string{"général", "aléatoire"} {
		c := &Channel{ID: randHex(8), ServerID: sv.ID, Name: cn}
		a.s.Channels[c.ID] = c
		sv.Channels = append(sv.Channels, c.ID)
	}
	a.s.Servers[sv.ID] = sv
	a.s.invIdx[sv.Invite] = sv.ID
	a.s.addMember(sv, u.ID, "owner")
	v := a.serverView(sv)
	a.hub.send([]string{u.ID}, "server.update", v)
	writeJSON(w, v)
}

func (a *App) joinServer(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct{ Code string }
	if !readJSON(w, r, &in) {
		return
	}
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	if i := strings.LastIndex(code, "/"); i >= 0 {
		code = code[i+1:]
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[a.s.invIdx[code]]
	if sv == nil {
		fail(w, 404, "Invitation invalide")
		return
	}
	if _, in := sv.Members[u.ID]; !in {
		if len(sv.Members) >= 5000 {
			fail(w, 400, "Ce serveur est plein")
			return
		}
		a.s.addMember(sv, u.ID, "member")
		a.pushServer(sv)
	}
	writeJSON(w, a.serverView(sv))
}

func (a *App) leaveServer(w http.ResponseWriter, r *http.Request, u *User) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	if sv == nil || sv.Members[u.ID] == "" {
		fail(w, 404, "Serveur introuvable")
		return
	}
	if sv.OwnerID == u.ID {
		fail(w, 400, "Le propriétaire ne peut pas quitter : supprime le serveur")
		return
	}
	a.s.removeMember(sv, u.ID)
	a.voiceEvictFromServer(sv, u.ID)
	a.hub.send([]string{u.ID}, "server.remove", map[string]string{"id": sv.ID})
	a.pushServer(sv)
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) deleteServer(w http.ResponseWriter, r *http.Request, u *User) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	if sv == nil || sv.OwnerID != u.ID {
		fail(w, 403, "Seul le propriétaire peut supprimer le serveur")
		return
	}
	to := a.members(sv)
	for _, cid := range sv.Channels {
		a.voiceDropChannel(cid)
		a.s.dropChannel(cid)
	}
	for uid := range sv.Members {
		delete(a.s.memberOf[uid], sv.ID)
	}
	delete(a.s.invIdx, sv.Invite)
	a.s.dropMedia("s", sv.ID)
	delete(a.s.Servers, sv.ID)
	a.s.touch()
	a.hub.send(to, "server.remove", map[string]string{"id": sv.ID})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) createChannel(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name string `json:"name"`
		Kind string `json:"kind"` // "" / "text" : textuel ; "voice" : vocal
	}
	if !readJSON(w, r, &in) {
		return
	}
	kind := KindText
	if in.Kind == KindVoice {
		kind = KindVoice
	}
	name := normChannel(in.Name)
	if name == "" {
		fail(w, 400, "Nom de salon invalide")
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	if sv == nil || !sv.has(u.ID, PermChannels) {
		fail(w, 403, "Permission refusée")
		return
	}
	if len(sv.Channels) >= maxChannels {
		fail(w, 400, "Limite de salons atteinte")
		return
	}
	c := &Channel{ID: randHex(8), ServerID: sv.ID, Name: name, Kind: kind}
	a.s.Channels[c.ID] = c
	sv.Channels = append(sv.Channels, c.ID)
	a.s.touch()
	a.pushServer(sv)
	writeJSON(w, ChannelView{c.ID, c.Name, c.Topic, c.Kind})
}

func (a *App) deleteChannel(w http.ResponseWriter, r *http.Request, u *User) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	c, sv, ok := a.chanAccess(u.ID, r.PathValue("id"))
	if !ok || sv == nil || !sv.has(u.ID, PermChannels) {
		fail(w, 403, "Permission refusée")
		return
	}
	if c.Kind != KindVoice {
		texts := 0
		for _, id := range sv.Channels {
			if ch := a.s.Channels[id]; ch != nil && ch.Kind != KindVoice {
				texts++
			}
		}
		if texts <= 1 {
			fail(w, 400, "Un serveur doit garder au moins un salon textuel")
			return
		}
	}
	a.voiceDropChannel(c.ID)
	for i, id := range sv.Channels {
		if id == c.ID {
			sv.Channels = append(sv.Channels[:i], sv.Channels[i+1:]...)
			break
		}
	}
	a.s.dropChannel(c.ID)
	a.pushServer(sv)
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) kick(w http.ResponseWriter, r *http.Request, u *User) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	uid := r.PathValue("uid")
	if sv == nil {
		fail(w, 404, "Serveur introuvable")
		return
	}
	target := sv.Members[uid]
	if !sv.has(u.ID, PermKick) || target == "" || uid == u.ID || target == "owner" || !sv.outranks(u.ID, uid) {
		fail(w, 403, "Permission refusée")
		return
	}
	a.s.removeMember(sv, uid)
	a.voiceEvictFromServer(sv, uid)
	a.hub.send([]string{uid}, "server.remove", map[string]string{"id": sv.ID})
	a.pushServer(sv)
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------- Messages ----------

func (a *App) listMessages(w http.ResponseWriter, r *http.Request, u *User) {
	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	c, _, ok := a.chanAccess(u.ID, r.PathValue("id"))
	if !ok {
		fail(w, 404, "Salon introuvable")
		return
	}
	list := a.s.msgs[c.ID]
	end := len(list)
	if before := r.URL.Query().Get("before"); before != "" {
		end = 0
		for i := len(list) - 1; i >= 0; i-- {
			if list[i].ID == before {
				end = i
				break
			}
		}
	}
	out := make([]MessageView, 0, pageSize)
	// on remonte depuis `end` en sautant les messages supprimés
	var picked []*Message
	for i := end - 1; i >= 0 && len(picked) < pageSize; i-- {
		if !list[i].Deleted {
			picked = append(picked, list[i])
		}
	}
	for i := len(picked) - 1; i >= 0; i-- {
		out = append(out, a.messageView(picked[i]))
	}
	writeJSON(w, map[string]any{"messages": out, "more": len(picked) == pageSize})
}

func (a *App) sendMessage(w http.ResponseWriter, r *http.Request, u *User) {
	if !a.rl.Allow("msg:"+u.ID, 10, 10*time.Second) {
		fail(w, 429, "Doucement ! Tu envoies trop de messages")
		return
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		a.sendWithFiles(w, r, u) // message avec pièces jointes
		return
	}
	var in struct {
		Content string `json:"content"`
		ReplyTo string `json:"reply_to"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	content := strings.TrimSpace(in.Content)
	if content == "" || utf8.RuneCountInString(content) > maxMsgLen {
		fail(w, 400, fmt.Sprintf("Message vide ou trop long (max %d)", maxMsgLen))
		return
	}
	a.postMessage(w, u, r.PathValue("id"), content, in.ReplyTo, nil)
}

// postMessage enregistre et diffuse un message (texte et/ou pièces jointes déjà écrites sur le disque).
// Retourne false si le message n'a pas été créé : l'appelant doit alors supprimer les fichiers.
func (a *App) postMessage(w http.ResponseWriter, u *User, cid, content, replyTo string, files []Attachment) bool {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	c, sv, ok := a.chanAccess(u.ID, cid)
	if !ok {
		fail(w, 404, "Salon introuvable")
		return false
	}
	if c.Kind == KindVoice {
		fail(w, 400, "Les salons vocaux n'ont pas de messages")
		return false
	}
	m := &Message{ID: randHex(8), ChannelID: c.ID, AuthorID: u.ID, Content: content, TS: time.Now().UnixMilli(), Files: files}
	if replyTo != "" {
		if p := a.s.msgIdx[replyTo]; p != nil && p.ChannelID == c.ID && !p.Deleted {
			m.ReplyTo = p.ID
		}
	}
	a.s.msgs[c.ID] = append(a.s.msgs[c.ID], m)
	a.s.msgIdx[m.ID] = m
	for _, f := range files {
		a.s.attIdx[f.ID] = m
	}
	a.s.journal(journalOp{Op: "new", M: m})
	v := a.messageView(m)
	a.hub.send(a.audience(c, sv), "message.new", v)
	writeJSON(w, v)
	return true
}

func (a *App) typing(w http.ResponseWriter, r *http.Request, u *User) {
	if !a.rl.Allow("typ:"+u.ID, 4, 10*time.Second) {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	c, sv, ok := a.chanAccess(u.ID, r.PathValue("id"))
	if !ok {
		fail(w, 404, "Salon introuvable")
		return
	}
	a.hub.send(a.audience(c, sv), "typing", map[string]string{"channel_id": c.ID, "user_id": u.ID, "name": shownName(u)})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) editMessage(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct{ Content string }
	if !readJSON(w, r, &in) {
		return
	}
	content := strings.TrimSpace(in.Content)
	if content == "" || utf8.RuneCountInString(content) > maxMsgLen {
		fail(w, 400, "Message vide ou trop long")
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	m := a.s.msgIdx[r.PathValue("id")]
	if m == nil || m.Deleted || m.AuthorID != u.ID {
		fail(w, 403, "Permission refusée")
		return
	}
	c, sv, ok := a.chanAccess(u.ID, m.ChannelID)
	if !ok {
		fail(w, 403, "Permission refusée")
		return
	}
	m.Content, m.Edited = content, true
	a.s.journal(journalOp{Op: "edit", ID: m.ID, Content: content})
	a.hub.send(a.audience(c, sv), "message.edit", a.messageView(m))
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) deleteMessage(w http.ResponseWriter, r *http.Request, u *User) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	m := a.s.msgIdx[r.PathValue("id")]
	if m == nil || m.Deleted {
		fail(w, 404, "Message introuvable")
		return
	}
	c, sv, ok := a.chanAccess(u.ID, m.ChannelID)
	if !ok || (m.AuthorID != u.ID && (sv == nil || !sv.has(u.ID, PermMessages))) {
		fail(w, 403, "Permission refusée")
		return
	}
	m.Deleted = true
	a.s.dropFiles(m)
	a.s.journal(journalOp{Op: "del", ID: m.ID})
	a.hub.send(a.audience(c, sv), "message.delete", map[string]string{"id": m.ID, "channel_id": c.ID})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) react(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Emoji string `json:"emoji"`
		On    bool   `json:"on"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Emoji == "" || len(in.Emoji) > 32 || utf8.RuneCountInString(in.Emoji) > 8 {
		fail(w, 400, "Émoji invalide")
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	m := a.s.msgIdx[r.PathValue("id")]
	if m == nil || m.Deleted {
		fail(w, 404, "Message introuvable")
		return
	}
	c, sv, ok := a.chanAccess(u.ID, m.ChannelID)
	if !ok {
		fail(w, 403, "Permission refusée")
		return
	}
	if m.Reactions == nil {
		m.Reactions = map[string][]string{}
	}
	users := m.Reactions[in.Emoji]
	has := -1
	for i, id := range users {
		if id == u.ID {
			has = i
		}
	}
	switch {
	case in.On && has < 0:
		if len(m.Reactions) >= 20 && users == nil {
			fail(w, 400, "Trop de réactions différentes")
			return
		}
		m.Reactions[in.Emoji] = append(users, u.ID)
	case !in.On && has >= 0:
		users = append(users[:has], users[has+1:]...)
		if len(users) == 0 {
			delete(m.Reactions, in.Emoji)
		} else {
			m.Reactions[in.Emoji] = users
		}
	}
	a.s.journal(journalOp{Op: "react", ID: m.ID, Reactions: m.Reactions})
	a.hub.send(a.audience(c, sv), "reaction", map[string]any{"channel_id": c.ID, "message_id": m.ID, "reactions": m.Reactions})
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------- Messages privés ----------

func (a *App) openDM(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct{ Username string }
	if !readJSON(w, r, &in) {
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	other := a.s.Users[a.s.byName[strings.ToLower(strings.TrimSpace(in.Username))]]
	if other == nil {
		fail(w, 404, "Utilisateur introuvable")
		return
	}
	if other.ID == u.ID {
		fail(w, 400, "Tu ne peux pas t'écrire à toi-même")
		return
	}
	if cid, ok := a.s.dmIdx[dmKey(u.ID, other.ID)]; ok {
		writeJSON(w, a.dmView(a.s.Channels[cid], u.ID))
		return
	}
	c := &Channel{ID: randHex(8), DM: []string{u.ID, other.ID}}
	a.s.Channels[c.ID] = c
	a.s.indexDM(c)
	a.s.touch()
	a.hub.send([]string{other.ID}, "dm.new", a.dmView(c, other.ID))
	v := a.dmView(c, u.ID)
	a.hub.send([]string{u.ID}, "dm.new", v)
	writeJSON(w, v)
}
