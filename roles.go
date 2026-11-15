package main

import (
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Permissions attribuables à un rôle. Le propriétaire a toutes les permissions, toujours.
const (
	PermChannels = "channels" // créer / supprimer des salons
	PermMessages = "messages" // supprimer les messages des autres
	PermKick     = "kick"     // expulser des membres
	PermRoles    = "roles"    // créer / modifier / attribuer des rôles
	PermServer   = "server"   // personnaliser le serveur (nom, icône, couleur, description)

	maxRoles = 25
)

// allPerms fixe aussi l'ordre canonique d'affichage.
var allPerms = []string{PermChannels, PermMessages, PermKick, PermRoles, PermServer}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

type RoleView struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Color string   `json:"color"`
	Perms []string `json:"perms"`
}

// ---------- Permissions et hiérarchie (le verrou du store doit être tenu) ----------

func (sv *Server) roleIndex(id string) int {
	for i, r := range sv.Roles {
		if r.ID == id {
			return i
		}
	}
	return -1
}

func (sv *Server) isOwner(uid string) bool { return uid == sv.OwnerID }

// has : le propriétaire a tout ; sinon une permission vient de l'un des rôles du membre.
func (sv *Server) has(uid, perm string) bool {
	if _, member := sv.Members[uid]; !member {
		return false
	}
	if sv.isOwner(uid) {
		return true
	}
	for _, rid := range sv.MemberRoles[uid] {
		if i := sv.roleIndex(rid); i >= 0 {
			for _, p := range sv.Roles[i].Perms {
				if p == perm {
					return true
				}
			}
		}
	}
	return false
}

func (sv *Server) hasAll(uid string, perms []string) bool {
	for _, p := range perms {
		if !sv.has(uid, p) {
			return false
		}
	}
	return true
}

// rank : plus le nombre est petit, plus le membre est haut placé.
// Propriétaire = -1 ; sans rôle = nombre de rôles (sous tous les rôles).
func (sv *Server) rank(uid string) int {
	if sv.isOwner(uid) {
		return -1
	}
	best := len(sv.Roles)
	for _, rid := range sv.MemberRoles[uid] {
		if i := sv.roleIndex(rid); i >= 0 && i < best {
			best = i
		}
	}
	return best
}

// outranks : strictement au-dessus (à rang égal, personne n'a autorité sur l'autre).
func (sv *Server) outranks(a, b string) bool { return sv.rank(a) < sv.rank(b) }

// canManageRole : le rôle d'index idx est strictement sous le plus haut rôle de uid.
func (sv *Server) canManageRole(uid string, idx int) bool {
	if sv.isOwner(uid) {
		return true
	}
	return sv.has(uid, PermRoles) && sv.rank(uid) < idx
}

// ---------- Validation ----------

func cleanRoleName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > 24 {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}

func cleanColor(s string) (string, bool) {
	if !hexColor.MatchString(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// cleanPerms valide, déduplique et remet les permissions dans l'ordre canonique.
func cleanPerms(in []string) ([]string, bool) {
	set := map[string]bool{}
	for _, p := range in {
		ok := false
		for _, a := range allPerms {
			if p == a {
				ok = true
			}
		}
		if !ok {
			return nil, false
		}
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for _, a := range allPerms {
		if set[a] {
			out = append(out, a)
		}
	}
	return out, true
}

func (sv *Server) nameTaken(name, exceptID string) bool {
	for _, r := range sv.Roles {
		if r.ID != exceptID && strings.EqualFold(r.Name, name) {
			return true
		}
	}
	return false
}

func roleView(r *Role) RoleView {
	return RoleView{r.ID, r.Name, r.Color, append([]string{}, r.Perms...)}
}

// ---------- Endpoints ----------

func (a *App) createRole(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name  string   `json:"name"`
		Color string   `json:"color"`
		Perms []string `json:"perms"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name, ok := cleanRoleName(in.Name)
	if !ok {
		fail(w, 400, "Nom du rôle : 1 à 24 caractères")
		return
	}
	color, ok := cleanColor(in.Color)
	if !ok {
		fail(w, 400, "Couleur invalide")
		return
	}
	perms, ok := cleanPerms(in.Perms)
	if !ok {
		fail(w, 400, "Permission inconnue")
		return
	}
	if !a.rl.Allow("role:"+u.ID, 30, time.Minute) {
		fail(w, 429, "Doucement ! Trop de modifications de rôles")
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	if sv == nil || !sv.has(u.ID, PermRoles) {
		fail(w, 403, "Permission refusée")
		return
	}
	if len(sv.Roles) >= maxRoles {
		fail(w, 400, "Limite de rôles atteinte")
		return
	}
	if sv.nameTaken(name, "") {
		fail(w, 409, "Un rôle porte déjà ce nom")
		return
	}
	if !sv.hasAll(u.ID, perms) {
		fail(w, 403, "Tu ne peux donner que des permissions que tu possèdes")
		return
	}
	role := &Role{ID: randHex(4), Name: name, Color: color, Perms: perms}
	sv.Roles = append(sv.Roles, role) // toujours créé tout en bas de la hiérarchie
	a.s.touch()
	a.pushServer(sv)
	writeJSON(w, roleView(role))
}

func (a *App) updateRole(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name  *string   `json:"name"`
		Color *string   `json:"color"`
		Perms *[]string `json:"perms"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var (
		name, color string
		perms       []string
		ok          bool
	)
	if in.Name != nil {
		if name, ok = cleanRoleName(*in.Name); !ok {
			fail(w, 400, "Nom du rôle : 1 à 24 caractères")
			return
		}
	}
	if in.Color != nil {
		if color, ok = cleanColor(*in.Color); !ok {
			fail(w, 400, "Couleur invalide")
			return
		}
	}
	if in.Perms != nil {
		if perms, ok = cleanPerms(*in.Perms); !ok {
			fail(w, 400, "Permission inconnue")
			return
		}
	}
	if !a.rl.Allow("role:"+u.ID, 30, time.Minute) {
		fail(w, 429, "Doucement ! Trop de modifications de rôles")
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	if sv == nil {
		fail(w, 404, "Serveur introuvable")
		return
	}
	idx := sv.roleIndex(r.PathValue("rid"))
	if idx < 0 {
		fail(w, 404, "Rôle introuvable")
		return
	}
	if !sv.canManageRole(u.ID, idx) {
		fail(w, 403, "Permission refusée")
		return
	}
	role := sv.Roles[idx]
	if in.Name != nil && sv.nameTaken(name, role.ID) {
		fail(w, 409, "Un rôle porte déjà ce nom")
		return
	}
	if in.Perms != nil {
		// on ne peut ajouter que des permissions qu'on possède soi-même (pas d'escalade)
		have := map[string]bool{}
		for _, p := range role.Perms {
			have[p] = true
		}
		for _, p := range perms {
			if !have[p] && !sv.has(u.ID, p) {
				fail(w, 403, "Tu ne peux donner que des permissions que tu possèdes")
				return
			}
		}
		role.Perms = perms
	}
	if in.Name != nil {
		role.Name = name
	}
	if in.Color != nil {
		role.Color = color
	}
	a.s.touch()
	a.pushServer(sv)
	writeJSON(w, roleView(role))
}

func (a *App) deleteRole(w http.ResponseWriter, r *http.Request, u *User) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	if sv == nil {
		fail(w, 404, "Serveur introuvable")
		return
	}
	idx := sv.roleIndex(r.PathValue("rid"))
	if idx < 0 {
		fail(w, 404, "Rôle introuvable")
		return
	}
	if !sv.canManageRole(u.ID, idx) {
		fail(w, 403, "Permission refusée")
		return
	}
	rid := sv.Roles[idx].ID
	sv.Roles = append(sv.Roles[:idx], sv.Roles[idx+1:]...)
	for uid, list := range sv.MemberRoles {
		kept := list[:0]
		for _, id := range list {
			if id != rid {
				kept = append(kept, id)
			}
		}
		if len(kept) == 0 {
			delete(sv.MemberRoles, uid)
		} else {
			sv.MemberRoles[uid] = kept
		}
	}
	a.s.touch()
	a.pushServer(sv)
	writeJSON(w, map[string]bool{"ok": true})
}

// moveRole monte (dir = -1) ou descend (dir = 1) un rôle dans la hiérarchie.
func (a *App) moveRole(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Dir int `json:"dir"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Dir != -1 && in.Dir != 1 {
		fail(w, 400, "Direction invalide")
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	if sv == nil {
		fail(w, 404, "Serveur introuvable")
		return
	}
	i := sv.roleIndex(r.PathValue("rid"))
	if i < 0 {
		fail(w, 404, "Rôle introuvable")
		return
	}
	j := i + in.Dir
	if j < 0 || j >= len(sv.Roles) {
		fail(w, 400, "Déjà au bout de la hiérarchie")
		return
	}
	// les deux rôles échangés doivent rester sous le plus haut rôle de l'acteur
	if !sv.canManageRole(u.ID, i) || !sv.canManageRole(u.ID, j) {
		fail(w, 403, "Permission refusée")
		return
	}
	sv.Roles[i], sv.Roles[j] = sv.Roles[j], sv.Roles[i]
	a.s.touch()
	a.pushServer(sv)
	writeJSON(w, map[string]bool{"ok": true})
}

// setMemberRole donne ou retire un rôle à un membre.
func (a *App) setMemberRole(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		RoleID string `json:"role_id"`
		On     bool   `json:"on"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !a.rl.Allow("role:"+u.ID, 30, time.Minute) {
		fail(w, 429, "Doucement ! Trop de modifications de rôles")
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	uid := r.PathValue("uid")
	if sv == nil {
		fail(w, 404, "Serveur introuvable")
		return
	}
	if _, member := sv.Members[uid]; !member {
		fail(w, 400, "Membre invalide")
		return
	}
	idx := sv.roleIndex(in.RoleID)
	if idx < 0 {
		fail(w, 404, "Rôle introuvable")
		return
	}
	if !sv.canManageRole(u.ID, idx) {
		fail(w, 403, "Permission refusée")
		return
	}
	if !sv.isOwner(u.ID) {
		// hors propriétaire : jamais sur soi-même ni sur un égal / supérieur…
		if uid == u.ID || !sv.outranks(u.ID, uid) {
			fail(w, 403, "Permission refusée")
			return
		}
		// …et on n'attribue que des rôles dont on possède toutes les permissions
		if in.On && !sv.hasAll(u.ID, sv.Roles[idx].Perms) {
			fail(w, 403, "Tu ne peux attribuer que des rôles dont les permissions sont incluses dans les tiennes")
			return
		}
	}
	list := sv.MemberRoles[uid]
	has := -1
	for i, id := range list {
		if id == in.RoleID {
			has = i
		}
	}
	switch {
	case in.On && has < 0:
		sv.MemberRoles[uid] = append(list, in.RoleID)
	case !in.On && has >= 0:
		list = append(list[:has], list[has+1:]...)
		if len(list) == 0 {
			delete(sv.MemberRoles, uid)
		} else {
			sv.MemberRoles[uid] = list
		}
	default:
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	a.s.touch()
	a.pushServer(sv)
	writeJSON(w, map[string]bool{"ok": true})
}
