package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// ---------- Modèles persistés ----------

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Hash     string `json:"hash"`
	Salt     string `json:"salt"`
	Color    string `json:"color"`
	Created  int64  `json:"created"`

	// Profil (tout est facultatif)
	Display   string `json:"display,omitempty"`    // nom affiché, distinct du pseudo de connexion
	Bio       string `json:"bio,omitempty"`        // « À propos de moi »
	Status    string `json:"status,omitempty"`     // statut personnalisé
	Banner    string `json:"banner,omitempty"`     // couleur de bannière #rrggbb
	AvatarVer int64  `json:"avatar_ver,omitempty"` // 0 = pas d'image ; sinon version (horodatage) du fichier
}

type Channel struct {
	ID       string   `json:"id"`
	ServerID string   `json:"server_id,omitempty"`
	Name     string   `json:"name,omitempty"`
	Topic    string   `json:"topic,omitempty"`
	Kind     string   `json:"kind,omitempty"` // "" = textuel, "voice" = salon vocal
	DM       []string `json:"dm,omitempty"`   // 2 user IDs pour un message privé
}

// Role est un rôle personnalisé d'un serveur : un nom, une couleur et des permissions.
// La hiérarchie est l'ordre de Server.Roles (index 0 = le plus haut).
type Role struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Color string   `json:"color"` // #rrggbb
	Perms []string `json:"perms"`
}

type Server struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	OwnerID     string              `json:"owner_id"`
	Invite      string              `json:"invite"`
	Members     map[string]string   `json:"members"` // userID -> owner|member (« admin » n'existe que dans les anciennes sauvegardes)
	Channels    []string            `json:"channels"`
	Created     int64               `json:"created"`
	Roles       []*Role             `json:"roles"`        // du plus haut au plus bas
	MemberRoles map[string][]string `json:"member_roles"` // userID -> IDs de rôles

	// Apparence (tout est facultatif)
	Description string `json:"description,omitempty"`
	Accent      string `json:"accent,omitempty"`   // couleur #rrggbb
	IconVer     int64  `json:"icon_ver,omitempty"` // 0 = pas d'icône ; sinon version du fichier
}

type Message struct {
	ID        string              `json:"id"`
	ChannelID string              `json:"channel_id"`
	AuthorID  string              `json:"author_id"`
	Content   string              `json:"content"`
	TS        int64               `json:"ts"`
	Edited    bool                `json:"edited,omitempty"`
	ReplyTo   string              `json:"reply_to,omitempty"`
	Reactions map[string][]string `json:"reactions,omitempty"`
	Files     []Attachment        `json:"files,omitempty"` // pièces jointes (images, MP3, ZIP…)
	Deleted   bool                `json:"-"`
}

type Session struct {
	UserID string `json:"user_id"`
	Exp    int64  `json:"exp"`
}

type snapshot struct {
	Users    map[string]*User    `json:"users"`
	Servers  map[string]*Server  `json:"servers"`
	Channels map[string]*Channel `json:"channels"`
	Sessions map[string]Session  `json:"sessions"`
}

type persistentPayload struct {
	Snapshot snapshot   `json:"snapshot"`
	Messages []*Message `json:"messages"`
}

type journalOp struct {
	Op        string              `json:"op"` // new | edit | del | react
	M         *Message            `json:"m,omitempty"`
	ID        string              `json:"id,omitempty"`
	Content   string              `json:"c,omitempty"`
	Reactions map[string][]string `json:"r,omitempty"`
}

// ---------- Store ----------

type Store struct {
	mu  sync.RWMutex
	dir string
	db  *sql.DB
	storage *ObjectStorage

	Users    map[string]*User
	Servers  map[string]*Server
	Channels map[string]*Channel
	Sessions map[string]Session

	msgs     map[string][]*Message // channelID -> messages (ordre chronologique)
	msgIdx   map[string]*Message
	attIdx   map[string]*Message // ID de pièce jointe -> message
	byName   map[string]string   // username (minuscule) -> userID
	invIdx   map[string]string   // code d'invitation -> serverID
	dmIdx    map[string]string   // "a|b" -> channelID
	userDMs  map[string][]string
	memberOf map[string]map[string]struct{} // userID -> set de serverID

	dirty bool
	jf    *os.File
	jw    *bufio.Writer
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	backupData(dir)
	s := &Store{
		dir:      dir,
		Users:    map[string]*User{},
		Servers:  map[string]*Server{},
		Channels: map[string]*Channel{},
		Sessions: map[string]Session{},
		msgs:     map[string][]*Message{},
		msgIdx:   map[string]*Message{},
		attIdx:   map[string]*Message{},
		byName:   map[string]string{},
		invIdx:   map[string]string{},
		dmIdx:    map[string]string{},
		userDMs:  map[string][]string{},
		memberOf: map[string]map[string]struct{}{},
	}

	// PostgreSQL est la source de vérité lorsque DATABASE_URL est défini.
	// Si la table est vide, les anciennes données locales sont chargées puis migrées.
	var persisted *persistentPayload
	if dsn := strings.TrimSpace(os.Getenv("DATABASE_URL")); dsn != "" {
		db, err := sql.Open("pgx", dsn)
		if err != nil { return nil, err }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		db.SetMaxOpenConns(5)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(30 * time.Minute)
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			return nil, fmt.Errorf("connexion PostgreSQL: %w", err)
		}
		_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS distro_persistence (
			id SMALLINT PRIMARY KEY CHECK (id = 1),
			payload JSONB NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("création de la table de persistance: %w", err)
		}
		var raw []byte
		err = db.QueryRowContext(ctx, "SELECT payload FROM distro_persistence WHERE id = 1").Scan(&raw)
		if err == nil {
			var p persistentPayload
			if err := json.Unmarshal(raw, &p); err != nil {
				db.Close()
				return nil, fmt.Errorf("lecture des données PostgreSQL: %w", err)
			}
			persisted = &p
		} else if err != sql.ErrNoRows {
			db.Close()
			return nil, fmt.Errorf("lecture PostgreSQL: %w", err)
		}
		s.db = db
		storage, err := newObjectStorage()
		if err != nil { db.Close(); return nil, err }
		s.storage = storage
	}

	if persisted != nil {
		snap := persisted.Snapshot
		if snap.Users != nil { s.Users = snap.Users }
		if snap.Servers != nil { s.Servers = snap.Servers }
		if snap.Channels != nil { s.Channels = snap.Channels }
		if snap.Sessions != nil { s.Sessions = snap.Sessions }
	} else if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		var snap snapshot
		if err := json.Unmarshal(b, &snap); err != nil {
			if s.db != nil { s.db.Close() }
			return nil, err
		}
		if snap.Users != nil { s.Users = snap.Users }
		if snap.Servers != nil { s.Servers = snap.Servers }
		if snap.Channels != nil { s.Channels = snap.Channels }
		if snap.Sessions != nil { s.Sessions = snap.Sessions }
	}
	for _, u := range s.Users {
		s.byName[strings.ToLower(u.Username)] = u.ID
	}
	for _, sv := range s.Servers {
		s.invIdx[sv.Invite] = sv.ID
		for uid := range sv.Members {
			s.link(uid, sv.ID)
		}
		s.migrateRoles(sv)
	}
	for _, c := range s.Channels {
		if len(c.DM) == 2 {
			s.indexDM(c)
		}
	}

	if persisted != nil {
		for _, m := range persisted.Messages {
			if m != nil { s.replay(&journalOp{Op: "new", M: m}) }
		}
	} else if f, err := os.Open(filepath.Join(dir, "messages.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			var op journalOp
			if json.Unmarshal(sc.Bytes(), &op) != nil { continue }
			s.replay(&op)
		}
		f.Close()
	}
	if err := s.compact(); err != nil {
		return nil, err
	}
	s.sweepFiles()
	if s.storage != nil {
		if err := s.migrateLocalObjects(); err != nil { if s.db != nil { s.db.Close() }; return nil, fmt.Errorf("migration des fichiers vers Supabase Storage: %w", err) }
	}
	if s.db != nil && persisted == nil { s.dirty = true }
	return s, nil
}

// migrateRoles met à niveau un serveur créé avant les rôles personnalisés :
// les anciens « admin » deviennent membres d'un vrai rôle « Admin » aux mêmes pouvoirs.
func (s *Store) migrateRoles(sv *Server) {
	if sv.MemberRoles == nil {
		sv.MemberRoles = map[string][]string{}
		s.dirty = true
	}
	if sv.Roles != nil {
		return
	}
	sv.Roles = []*Role{}
	s.dirty = true
	var admins []string
	for uid, r := range sv.Members {
		if r == "admin" {
			admins = append(admins, uid)
		}
	}
	if len(admins) == 0 {
		return
	}
	r := &Role{ID: randHex(4), Name: "Admin", Color: "#38bdf8", Perms: []string{PermChannels, PermMessages, PermKick}}
	sv.Roles = append(sv.Roles, r)
	for _, uid := range admins {
		sv.Members[uid] = "member"
		sv.MemberRoles[uid] = []string{r.ID}
	}
}

func (s *Store) replay(op *journalOp) {
	switch op.Op {
	case "new":
		if op.M == nil || s.Channels[op.M.ChannelID] == nil {
			return
		}
		s.msgs[op.M.ChannelID] = append(s.msgs[op.M.ChannelID], op.M)
		s.msgIdx[op.M.ID] = op.M
		for _, f := range op.M.Files {
			s.attIdx[f.ID] = op.M
		}
	case "edit":
		if m := s.msgIdx[op.ID]; m != nil {
			m.Content, m.Edited = op.Content, true
		}
	case "del":
		if m := s.msgIdx[op.ID]; m != nil {
			m.Deleted = true
		}
	case "react":
		if m := s.msgIdx[op.ID]; m != nil {
			m.Reactions = op.Reactions
		}
	}
}

// compact réécrit le journal sans les messages supprimés puis l'ouvre en ajout.
func (s *Store) compact() error {
	tmp := filepath.Join(s.dir, "messages.jsonl.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for cid, list := range s.msgs {
		kept := list[:0]
		for _, m := range list {
			if m.Deleted {
				delete(s.msgIdx, m.ID)
				s.dropFiles(m)
				continue
			}
			kept = append(kept, m)
			enc.Encode(journalOp{Op: "new", M: m})
		}
		s.msgs[cid] = kept
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	f.Sync()
	f.Close()
	final := filepath.Join(s.dir, "messages.jsonl")
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	s.jf, err = os.OpenFile(final, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	s.jw = bufio.NewWriter(s.jf)
	return nil
}

// journal : le verrou d'écriture doit être tenu.
func (s *Store) journal(op journalOp) {
	b, _ := json.Marshal(op)
	s.jw.Write(b)
	s.jw.WriteByte('\n')
	s.jw.Flush()
	s.touch()
}

func (s *Store) touch() { s.dirty = true }

// writeFileSync écrit puis force les octets sur le disque (sinon une coupure de courant peut laisser un fichier vide).
func writeFileSync(path string, b []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// backupData copie state.json et messages.jsonl dans DATA_DIR/backups à chaque démarrage
// et ne garde que les 10 dernières copies de chaque.
func backupData(dir string) {
	bdir := filepath.Join(dir, "backups")
	stamp := time.Now().Format("20060102-150405")
	for _, name := range []string{"state.json", "messages.jsonl"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(b) == 0 {
			continue
		}
		os.MkdirAll(bdir, 0o755)
		os.WriteFile(filepath.Join(bdir, stamp+"-"+name), b, 0o600)
	}
	for _, suffix := range []string{"-state.json", "-messages.jsonl"} {
		list, _ := filepath.Glob(filepath.Join(bdir, "*"+suffix))
		sort.Strings(list)
		for len(list) > 10 {
			os.Remove(list[0])
			list = list[1:]
		}
	}
}

// Save écrit l'instantané (hors messages) si quelque chose a changé.
func (s *Store) Save() error {
	s.mu.Lock()
	if s.jf != nil { s.jf.Sync() }
	now := time.Now().Unix()
	for k, v := range s.Sessions {
		if v.Exp < now { delete(s.Sessions, k); s.dirty = true }
	}
	if !s.dirty { s.mu.Unlock(); return nil }

	var b []byte
	var err error
	if s.db != nil {
		messages := make([]*Message, 0, len(s.msgIdx))
		for _, list := range s.msgs {
			for _, m := range list {
				if m != nil && !m.Deleted { messages = append(messages, m) }
			}
		}
		payload := persistentPayload{Snapshot: snapshot{s.Users, s.Servers, s.Channels, s.Sessions}, Messages: messages}
		b, err = json.Marshal(payload)
	} else {
		b, err = json.Marshal(snapshot{s.Users, s.Servers, s.Channels, s.Sessions})
	}
	if err != nil { s.mu.Unlock(); return err }
	db := s.db
	s.dirty = false
	s.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := db.ExecContext(ctx, `INSERT INTO distro_persistence (id, payload, updated_at)
			VALUES (1, $1::jsonb, NOW())
			ON CONFLICT (id) DO UPDATE SET payload = EXCLUDED.payload, updated_at = NOW()`, string(b))
		if err != nil {
			s.mu.Lock(); s.dirty = true; s.mu.Unlock()
			return fmt.Errorf("sauvegarde PostgreSQL: %w", err)
		}
		return nil
	}

	tmp := filepath.Join(s.dir, "state.json.tmp")
	if err := writeFileSync(tmp, b, 0o600); err != nil {
		s.mu.Lock(); s.dirty = true; s.mu.Unlock()
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, "state.json")); err != nil {
		s.mu.Lock(); s.dirty = true; s.mu.Unlock()
		return err
	}
	return nil
}

func (s *Store) Close() {
	s.Save()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jw != nil {
		s.jw.Flush()
		s.jf.Close()
	}
	if s.db != nil { s.db.Close() }
}

// ---------- Index (verrou d'écriture tenu) ----------

func (s *Store) link(uid, sid string) {
	m := s.memberOf[uid]
	if m == nil {
		m = map[string]struct{}{}
		s.memberOf[uid] = m
	}
	m[sid] = struct{}{}
}

func (s *Store) addMember(sv *Server, uid, role string) {
	sv.Members[uid] = role
	s.link(uid, sv.ID)
	s.touch()
}

func (s *Store) removeMember(sv *Server, uid string) {
	delete(sv.Members, uid)
	delete(sv.MemberRoles, uid)
	delete(s.memberOf[uid], sv.ID)
	s.touch()
}

func dmKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

func (s *Store) indexDM(c *Channel) {
	s.dmIdx[dmKey(c.DM[0], c.DM[1])] = c.ID
	s.userDMs[c.DM[0]] = append(s.userDMs[c.DM[0]], c.ID)
	if c.DM[1] != c.DM[0] {
		s.userDMs[c.DM[1]] = append(s.userDMs[c.DM[1]], c.ID)
	}
}

func (s *Store) dropChannel(cid string) {
	for _, m := range s.msgs[cid] {
		s.dropFiles(m)
		delete(s.msgIdx, m.ID)
	}
	delete(s.msgs, cid)
	delete(s.Channels, cid)
	s.touch()
}
