package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Personnalisation : profil des utilisateurs, apparence des serveurs, sujets de salons.
// Les images (avatars, icônes) sont redimensionnées par l'interface avant l'envoi, puis stockées
// dans DATA_DIR/media ; elles sont servies par des URL versionnées (?v=…) donc cachables à vie.

const (
	maxMediaBytes = 200 << 10 // 200 Ko par image
	maxDisplay    = 32
	maxBio        = 190
	maxStatus     = 64
	maxServerDesc = 190
	maxTopic      = 120
)

// ---------- Validation ----------

// isBidi : les caractères de contrôle bidirectionnel permettent d'usurper un pseudo ou de retourner du texte.
func isBidi(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// cleanLine : texte d'une seule ligne, sans caractère de contrôle ; le vide est autorisé.
func cleanLine(s string, limit int) (string, bool) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > limit {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) || isBidi(r) {
			return "", false
		}
	}
	return s, true
}

// cleanMultiline : comme cleanLine mais avec au plus 6 lignes.
func cleanMultiline(s string, limit int) (string, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if utf8.RuneCountInString(s) > limit || strings.Count(s, "\n") > 5 {
		return "", false
	}
	for _, r := range s {
		if (r != '\n' && unicode.IsControl(r)) || isBidi(r) {
			return "", false
		}
	}
	return s, true
}

// cleanOptColor : couleur #rrggbb, ou vide pour « aucune ».
func cleanOptColor(s string) (string, bool) {
	if s == "" {
		return "", true
	}
	return cleanColor(s)
}

// shownName : nom affiché (le pseudo de connexion reste l'identifiant unique).
func shownName(u *User) string {
	if u.Display != "" {
		return u.Display
	}
	return u.Username
}

// ---------- Fichiers image ----------

func (s *Store) mediaFile(kind, id string) string {
	return filepath.Join(s.dir, "media", kind+"-"+id)
}

func (s *Store) saveMedia(kind, id string, data []byte) error {
	if err := os.MkdirAll(filepath.Join(s.dir, "media"), 0o755); err != nil {
		return err
	}
	p := s.mediaFile(kind, id)
	tmp := p + ".tmp"
	if err := writeFileSync(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil { return err }
	if s.storage != nil {
		if err := s.storage.Upload("media/"+kind+"-"+id, data, http.DetectContentType(data)); err != nil { return err }
	}
	return nil
}

func (s *Store) dropMedia(kind, id string) {
	os.Remove(s.mediaFile(kind, id))
	if s.storage != nil { _ = s.storage.Delete("media/"+kind+"-"+id) }
}

// readImage lit le corps de la requête et n'accepte que PNG, JPEG ou WebP (jamais de SVG : il peut contenir du script).
func readImage(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMediaBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, 413, "Image trop lourde (200 Ko maximum)")
		return nil, false
	}
	switch http.DetectContentType(data) {
	case "image/png", "image/jpeg", "image/webp":
		return data, true
	}
	fail(w, 415, "Format d'image non pris en charge (PNG, JPEG ou WebP)")
	return nil, false
}

func (a *App) serveMedia(w http.ResponseWriter, r *http.Request, kind, id string) {
	b, err := os.ReadFile(a.s.mediaFile(kind, id))
	if err != nil && a.s.storage != nil {
		b, err = a.s.storage.Download("media/"+kind+"-"+id)
	}
	if err != nil { http.NotFound(w, r); return }
	w.Header().Set("Content-Type", http.DetectContentType(b))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Write(b)
}

// pushUser prévient tous les clients concernés (l'intéressé et les gens avec qui il partage un serveur ou un MP).
// Le verrou du store doit être tenu.
func (a *App) pushUser(u *User) {
	to := append(a.peers(u.ID), u.ID)
	a.hub.send(to, "user.update", a.userView(u))
}

// ---------- Profil ----------

func (a *App) updateProfile(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Display *string `json:"display"`
		Bio     *string `json:"bio"`
		Status  *string `json:"status"`
		Color   *string `json:"color"`
		Banner  *string `json:"banner"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var (
		display, bio, status, color, banner string
		ok                                  bool
	)
	if in.Display != nil {
		if display, ok = cleanLine(*in.Display, maxDisplay); !ok {
			fail(w, 400, "Nom affiché : 32 caractères maximum, sans caractère spécial")
			return
		}
	}
	if in.Status != nil {
		if status, ok = cleanLine(*in.Status, maxStatus); !ok {
			fail(w, 400, "Statut : 64 caractères maximum, sur une seule ligne")
			return
		}
	}
	if in.Bio != nil {
		if bio, ok = cleanMultiline(*in.Bio, maxBio); !ok {
			fail(w, 400, "À propos : 190 caractères et 6 lignes maximum")
			return
		}
	}
	if in.Color != nil {
		if color, ok = cleanColor(*in.Color); !ok {
			fail(w, 400, "Couleur invalide")
			return
		}
	}
	if in.Banner != nil {
		if banner, ok = cleanOptColor(*in.Banner); !ok {
			fail(w, 400, "Couleur de bannière invalide")
			return
		}
	}
	if !a.rl.Allow("prof:"+u.ID, 20, time.Minute) {
		fail(w, 429, "Doucement ! Trop de modifications de profil")
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	if in.Display != nil {
		u.Display = display
	}
	if in.Status != nil {
		u.Status = status
	}
	if in.Bio != nil {
		u.Bio = bio
	}
	if in.Color != nil {
		u.Color = color
	}
	if in.Banner != nil {
		u.Banner = banner
	}
	a.s.touch()
	a.pushUser(u)
	writeJSON(w, a.userView(u))
}

func (a *App) setAvatar(w http.ResponseWriter, r *http.Request, u *User) {
	if !a.rl.Allow("media:"+u.ID, 10, time.Minute) {
		fail(w, 429, "Doucement ! Trop d'envois d'images")
		return
	}
	data, ok := readImage(w, r)
	if !ok {
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	if err := a.s.saveMedia("u", u.ID, data); err != nil {
		fail(w, 500, "Enregistrement de l'image impossible")
		return
	}
	u.AvatarVer = time.Now().UnixMilli()
	a.s.touch()
	a.pushUser(u)
	writeJSON(w, a.userView(u))
}

func (a *App) clearAvatar(w http.ResponseWriter, r *http.Request, u *User) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	if u.AvatarVer != 0 {
		a.s.dropMedia("u", u.ID)
		u.AvatarVer = 0
		a.s.touch()
		a.pushUser(u)
	}
	writeJSON(w, a.userView(u))
}

func (a *App) userAvatar(w http.ResponseWriter, r *http.Request, u *User) {
	id := r.PathValue("id")
	a.s.mu.RLock()
	t := a.s.Users[id]
	ok := t != nil && t.AvatarVer != 0
	a.s.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.serveMedia(w, r, "u", id)
}

// ---------- Serveur ----------

func (a *App) updateServer(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Accent      *string `json:"accent"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var (
		name, desc, accent string
		ok                 bool
	)
	if in.Name != nil {
		name, ok = cleanLine(*in.Name, 50)
		if !ok || utf8.RuneCountInString(name) < 2 {
			fail(w, 400, "Nom du serveur : 2 à 50 caractères")
			return
		}
	}
	if in.Description != nil {
		if desc, ok = cleanMultiline(*in.Description, maxServerDesc); !ok {
			fail(w, 400, "Description : 190 caractères et 6 lignes maximum")
			return
		}
	}
	if in.Accent != nil {
		if accent, ok = cleanOptColor(*in.Accent); !ok {
			fail(w, 400, "Couleur invalide")
			return
		}
	}
	if !a.rl.Allow("srv:"+u.ID, 20, time.Minute) {
		fail(w, 429, "Doucement ! Trop de modifications")
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	if sv == nil || !sv.has(u.ID, PermServer) {
		fail(w, 403, "Permission refusée")
		return
	}
	if in.Name != nil {
		sv.Name = name
	}
	if in.Description != nil {
		sv.Description = desc
	}
	if in.Accent != nil {
		sv.Accent = accent
	}
	a.s.touch()
	a.pushServer(sv)
	writeJSON(w, a.serverView(sv))
}

func (a *App) setServerIcon(w http.ResponseWriter, r *http.Request, u *User) {
	if !a.rl.Allow("media:"+u.ID, 10, time.Minute) {
		fail(w, 429, "Doucement ! Trop d'envois d'images")
		return
	}
	data, ok := readImage(w, r)
	if !ok {
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	if sv == nil || !sv.has(u.ID, PermServer) {
		fail(w, 403, "Permission refusée")
		return
	}
	if err := a.s.saveMedia("s", sv.ID, data); err != nil {
		fail(w, 500, "Enregistrement de l'image impossible")
		return
	}
	sv.IconVer = time.Now().UnixMilli()
	a.s.touch()
	a.pushServer(sv)
	writeJSON(w, a.serverView(sv))
}

func (a *App) clearServerIcon(w http.ResponseWriter, r *http.Request, u *User) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sv := a.s.Servers[r.PathValue("id")]
	if sv == nil || !sv.has(u.ID, PermServer) {
		fail(w, 403, "Permission refusée")
		return
	}
	if sv.IconVer != 0 {
		a.s.dropMedia("s", sv.ID)
		sv.IconVer = 0
		a.s.touch()
		a.pushServer(sv)
	}
	writeJSON(w, a.serverView(sv))
}

// serverIcon : réservé aux membres du serveur.
func (a *App) serverIcon(w http.ResponseWriter, r *http.Request, u *User) {
	id := r.PathValue("id")
	a.s.mu.RLock()
	sv := a.s.Servers[id]
	ok := sv != nil && sv.IconVer != 0
	if ok {
		_, ok = sv.Members[u.ID]
	}
	a.s.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.serveMedia(w, r, "s", id)
}

// ---------- Salon : nom et sujet ----------

func (a *App) updateChannel(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name  *string `json:"name"`
		Topic *string `json:"topic"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var name, topic string
	if in.Name != nil {
		if name = normChannel(*in.Name); name == "" {
			fail(w, 400, "Nom de salon invalide")
			return
		}
	}
	if in.Topic != nil {
		var ok bool
		if topic, ok = cleanLine(*in.Topic, maxTopic); !ok {
			fail(w, 400, "Sujet : 120 caractères maximum, sur une seule ligne")
			return
		}
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	c, sv, ok := a.chanAccess(u.ID, r.PathValue("id"))
	if !ok || sv == nil || !sv.has(u.ID, PermChannels) {
		fail(w, 403, "Permission refusée")
		return
	}
	if in.Name != nil {
		c.Name = name
	}
	if in.Topic != nil {
		c.Topic = topic
	}
	a.s.touch()
	a.pushServer(sv)
	writeJSON(w, ChannelView{c.ID, c.Name, c.Topic, c.Kind})
}
