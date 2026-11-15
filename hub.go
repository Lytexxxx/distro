package main

import (
	"encoding/json"
	"sync"
	"time"
)

type Event struct {
	T string `json:"t"`
	D any    `json:"d"`
}

// Hub distribue les évènements temps réel (SSE) aux connexions de chaque utilisateur.
type Hub struct {
	mu    sync.Mutex
	conns map[string]map[chan []byte]struct{}
}

func NewHub() *Hub { return &Hub{conns: map[string]map[chan []byte]struct{}{}} }

func (h *Hub) add(uid string) (chan []byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.conns[uid]
	first := len(m) == 0
	if m == nil {
		m = map[chan []byte]struct{}{}
		h.conns[uid] = m
	}
	c := make(chan []byte, 128)
	m[c] = struct{}{}
	return c, first
}

func (h *Hub) remove(uid string, c chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.conns[uid]
	if _, ok := m[c]; !ok {
		return
	}
	delete(m, c)
	close(c)
	if len(m) == 0 {
		delete(h.conns, uid)
	}
}

func (h *Hub) online(uid string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns[uid]) > 0
}

// send ne bloque jamais : un client trop lent est déconnecté et se resynchronisera à la reconnexion.
func (h *Hub) send(uids []string, t string, d any) {
	b, err := json.Marshal(Event{t, d})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, uid := range uids {
		m := h.conns[uid]
		for c := range m {
			select {
			case c <- b:
			default:
				delete(m, c)
				close(c)
			}
		}
		if len(m) == 0 {
			delete(h.conns, uid)
		}
	}
}

// Limiter : fenêtre glissante par clé.
type Limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func NewLimiter() *Limiter { return &Limiter{hits: map[string][]time.Time{}} }

func (l *Limiter) Allow(key string, n int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cut := now.Add(-per)
	h := l.hits[key]
	i := 0
	for i < len(h) && h[i].Before(cut) {
		i++
	}
	h = h[i:]
	if len(h) >= n {
		l.hits[key] = h
		return false
	}
	l.hits[key] = append(h, now)
	return true
}

func (l *Limiter) GC() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-10 * time.Minute)
	for k, h := range l.hits {
		if len(h) == 0 || h[len(h)-1].Before(cut) {
			delete(l.hits, k)
		}
	}
}
