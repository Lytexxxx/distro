package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Pièces jointes : images, audio (MP3…), vidéo, ZIP et n'importe quel autre fichier.
//
// Envoi    : POST /api/channels/{id}/messages en multipart/form-data (champs « content », « reply_to »
//            et un ou plusieurs « file »). Les fichiers sont écrits en flux sur le disque, jamais gardés en mémoire.
// Lecture  : GET /api/files/{id}[?dl=1], réservé aux gens qui ont accès au salon du message.
// Stockage : DATA_DIR/files/{id} (à sauvegarder avec le reste). Le fichier disparaît avec son message.
//
// Sécurité : le type de contenu n'est JAMAIS celui annoncé par le client. Il est déduit du contenu (sniffing)
// puis ramené à une courte liste de types sûrs ; tout le reste est servi en application/octet-stream
// avec Content-Disposition: attachment. Le SVG et le HTML ne sont donc jamais affichés dans la page.

const (
	maxFilesPerMsg   = 5
	defaultMaxUpload = 25 << 20 // 25 Mo par fichier (variable MAX_UPLOAD_MB)
	maxFileNameLen   = 120
)

var (
	errTooBig = errors.New("fichier trop lourd")
	errEmpty  = errors.New("fichier vide")
)
var hexID = regexp.MustCompile(`^[0-9a-f]{24}$`)

type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Kind string `json:"kind"`           // image | audio | video | file
	Type string `json:"type,omitempty"` // type MIME servi en ligne (vide pour « file »)
}

type FileView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Kind string `json:"kind"`
}

func fileViews(list []Attachment) []FileView {
	if len(list) == 0 {
		return nil
	}
	out := make([]FileView, len(list))
	for i, f := range list {
		out[i] = FileView{f.ID, f.Name, f.Size, f.Kind}
	}
	return out
}

// ---------- Stockage ----------

func (s *Store) filesDir() string { return filepath.Join(s.dir, "files") }

func (s *Store) fileFile(id string) string {
	if !hexID.MatchString(id) { // garde-fou : un ID ne doit jamais pouvoir sortir du dossier
		return filepath.Join(s.filesDir(), "invalid")
	}
	return filepath.Join(s.filesDir(), id)
}

// dropFiles supprime du disque les pièces jointes d'un message. Le verrou d'écriture doit être tenu.
func (s *Store) dropFiles(m *Message) {
	for _, f := range m.Files {
		os.Remove(s.fileFile(f.ID))
		if s.storage != nil { _ = s.storage.Delete("files/"+f.ID) }
		delete(s.attIdx, f.ID)
	}
}

// sweepFiles retire les restes d'envois interrompus (.part) et les fichiers qu'aucun message ne référence
// (plantage entre l'écriture du fichier et celle du journal), seulement s'ils ont plus de 24 h.
func (s *Store) sweepFiles() {
	list, err := os.ReadDir(s.filesDir())
	if err != nil {
		return
	}
	for _, e := range list {
		p := filepath.Join(s.filesDir(), e.Name())
		if strings.HasSuffix(e.Name(), ".part") {
			os.Remove(p)
			continue
		}
		if _, used := s.attIdx[e.Name()]; used {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			os.Remove(p)
		}
	}
}

// ---------- Nom et type ----------

// cleanFileName : nom sans chemin, sans caractère de contrôle ni de contrôle bidirectionnel, 120 caractères au plus
// (l'extension est conservée).
func cleanFileName(raw string) string {
	if i := strings.LastIndexAny(raw, `/\`); i >= 0 {
		raw = raw[i+1:]
	}
	var b strings.Builder
	for _, r := range raw {
		if unicode.IsControl(r) || isBidi(r) || r == utf8.RuneError || r == ' ' {
			r = ' '
		}
		b.WriteRune(r)
	}
	name := strings.TrimSpace(b.String())
	if r := []rune(name); len(r) > maxFileNameLen {
		ext := []rune(filepath.Ext(name))
		if len(ext) > 12 {
			ext = nil
		}
		name = string(r[:maxFileNameLen-len(ext)]) + string(ext)
	}
	if name == "" || name == "." || name == ".." {
		return "fichier"
	}
	return name
}

var audioExt = map[string]string{
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".flac": "audio/flac",
	".wav": "audio/wav", ".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/ogg",
}

// classify décide comment un fichier est présenté, d'après son contenu (512 premiers octets) et son extension.
// Seuls des types connus sont affichés en ligne ; le reste (zip, pdf, exe, svg, html…) est un simple téléchargement.
func classify(name string, head []byte) (kind, typ string) {
	ct := http.DetectContentType(head)
	ext := strings.ToLower(filepath.Ext(name))
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return "image", ct
	case "video/webm":
		return "video", ct
	}
	if t, ok := audioExt[ext]; ok {
		if strings.HasPrefix(ct, "audio/") || ct == "application/ogg" || ct == "application/octet-stream" || (ct == "video/mp4" && ext == ".m4a") {
			return "audio", t
		}
	}
	if ct == "video/mp4" || ((ext == ".mp4" || ext == ".m4v") && ct == "application/octet-stream" && len(head) >= 8 && bytes.Equal(head[4:8], []byte("ftyp"))) {
		return "video", "video/mp4"
	}
	return "file", ""
}

// storeUpload écrit une partie multipart sur le disque, au plus `limit` octets.
func (s *Store) storeUpload(part io.Reader, rawName string, limit int64) (*Attachment, error) {
	if err := os.MkdirAll(s.filesDir(), 0o755); err != nil {
		return nil, err
	}
	id := randHex(12)
	final := s.fileFile(id)
	tmp := final + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()

	head := make([]byte, 512)
	n, err := io.ReadFull(part, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	head = head[:n]
	total := int64(n)
	if _, err := f.Write(head); err != nil {
		return nil, err
	}
	rest, err := io.Copy(f, io.LimitReader(part, limit-total+1))
	if err != nil {
		return nil, err
	}
	total += rest
	if total > limit {
		return nil, errTooBig
	}
	if total == 0 {
		return nil, errEmpty
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return nil, err
	}
	ok = true
	name := cleanFileName(rawName)
	kind, typ := classify(name, head)
	return &Attachment{ID: id, Name: name, Size: total, Kind: kind, Type: typ}, nil
}

// ---------- Envoi (multipart) ----------

func mbString(n int64) string {
	if n%(1<<20) == 0 {
		return strconv.FormatInt(n>>20, 10) + " Mo"
	}
	return strconv.FormatInt(n>>10, 10) + " Ko"
}

func (a *App) sendWithFiles(w http.ResponseWriter, r *http.Request, u *User) {
	cid := r.PathValue("id")
	a.s.mu.RLock()
	_, _, ok := a.chanAccess(u.ID, cid)
	a.s.mu.RUnlock()
	if !ok { // on refuse avant de lire le moindre octet du corps
		fail(w, 404, "Salon introuvable")
		return
	}
	if !a.rl.Allow("up:"+u.ID, 8, time.Minute) {
		fail(w, 429, "Doucement ! Trop d'envois de fichiers")
		return
	}
	// un envoi peut durer longtemps : on remplace les délais globaux (30 s) pour cette requête seulement
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(20 * time.Minute)
	rc.SetReadDeadline(deadline)
	rc.SetWriteDeadline(deadline)
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxFilesPerMsg)*a.maxFile+(1<<20))

	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, "Requête invalide")
		return
	}
	var (
		content, replyTo string
		atts             []Attachment
	)
	cleanup := func() {
		for _, f := range atts {
			os.Remove(a.s.fileFile(f.ID))
			if a.s.storage != nil { _ = a.s.storage.Delete("files/"+f.ID) }
		}
	}
	abort := func(code int, msg string) { cleanup(); fail(w, code, msg) }

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				abort(413, "Envoi trop lourd")
			} else {
				abort(400, "Envoi interrompu")
			}
			return
		}
		switch part.FormName() {
		case "content", "reply_to":
			b, _ := io.ReadAll(io.LimitReader(part, 16<<10))
			if part.FormName() == "content" {
				content = string(b)
			} else {
				replyTo = strings.TrimSpace(string(b))
			}
		case "file":
			if len(atts) >= maxFilesPerMsg {
				abort(400, "5 fichiers au maximum par message")
				return
			}
			att, err := a.s.storeUpload(part, part.FileName(), a.maxFile)
			if err != nil {
				var mbe *http.MaxBytesError
				switch {
				case errors.Is(err, errTooBig) || errors.As(err, &mbe):
					abort(413, "Fichier trop lourd ("+mbString(a.maxFile)+" maximum)")
				case errors.Is(err, errEmpty):
					abort(400, "Fichier vide : "+cleanFileName(part.FileName()))
				default:
					abort(500, "Enregistrement du fichier impossible")
				}
				return
			}
			if a.s.storage != nil {
				data, readErr := os.ReadFile(a.s.fileFile(att.ID))
				if readErr == nil {
					ct := att.Type
					if ct == "" { ct = http.DetectContentType(data) }
					readErr = a.s.storage.Upload("files/"+att.ID, data, ct)
				}
				if readErr != nil {
					log.Printf("ERREUR UPLOAD SUPABASE STORAGE: %v", readErr)
					os.Remove(a.s.fileFile(att.ID))
					abort(502, "Envoi du fichier vers le stockage distant impossible")
					return
				}
			}
			atts = append(atts, *att)
		}
	}
	content = strings.TrimSpace(content)
	if utf8.RuneCountInString(content) > maxMsgLen || (content == "" && len(atts) == 0) {
		abort(400, "Message vide ou trop long")
		return
	}
	if !a.postMessage(w, u, cid, content, replyTo, atts) {
		cleanup()
	}
}

// ---------- Lecture ----------

func (a *App) serveAttachment(w http.ResponseWriter, r *http.Request, u *User) {
	id := r.PathValue("id")
	var (
		att   Attachment
		found bool
	)
	a.s.mu.RLock()
	if m := a.s.attIdx[id]; m != nil && !m.Deleted {
		if _, _, acc := a.chanAccess(u.ID, m.ChannelID); acc {
			for _, f := range m.Files {
				if f.ID == id {
					att, found = f, true
				}
			}
		}
	}
	a.s.mu.RUnlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(a.s.fileFile(id))
	var reader io.ReadSeeker
	var modTime time.Time
	if err == nil {
		defer f.Close()
		info, statErr := f.Stat()
		if statErr != nil { http.NotFound(w, r); return }
		reader, modTime = f, info.ModTime()
	} else if a.s.storage != nil {
		data, remoteErr := a.s.storage.Download("files/"+id)
		if remoteErr != nil { http.NotFound(w, r); return }
		reader, modTime = bytes.NewReader(data), time.Unix(0, 0)
	} else {
		http.NotFound(w, r)
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Minute))

	inline := att.Kind != "file" && r.URL.Query().Get("dl") != "1"
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	cd := mime.FormatMediaType(disp, map[string]string{"filename": att.Name})
	if cd == "" {
		cd = disp
	}
	typ := att.Type
	if typ == "" {
		typ = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", typ)
	h.Set("Content-Disposition", cd)
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox") // même ouvert seul, rien ne peut s'y exécuter
	http.ServeContent(w, r, "", modTime, reader)                  // gère Range : on peut avancer dans un MP3 ou une vidéo
}
