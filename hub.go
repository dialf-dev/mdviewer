package main

import (
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// hub fans out reload events to SSE subscribers per document, watching only
// the directories that currently have at least one open viewer (lazy
// watching keeps the inotify watch count proportional to open documents,
// not to the size of registered trees).
type hub struct {
	mu     sync.Mutex
	subs   map[string]map[chan struct{}]bool // docID -> subscriber channels
	refs   map[string]int                    // dir -> subscriber refcount
	timers map[string]*time.Timer            // docID -> debounce timer
	w      *fsnotify.Watcher
	idFor  func(path string) string
}

func newHub(idFor func(string) string) (*hub, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	h := &hub{
		subs:   make(map[string]map[chan struct{}]bool),
		refs:   make(map[string]int),
		timers: make(map[string]*time.Timer),
		w:      w,
		idFor:  idFor,
	}
	go h.loop()
	return h, nil
}

func (h *hub) subscribe(docID, dir string) chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[docID] == nil {
		h.subs[docID] = make(map[chan struct{}]bool)
	}
	h.subs[docID][ch] = true
	h.refs[dir]++
	if h.refs[dir] == 1 {
		if err := h.w.Add(dir); err != nil {
			log.Printf("watch %s: %v", dir, err)
		}
	}
	return ch
}

func (h *hub) unsubscribe(docID, dir string, ch chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.subs[docID]; set != nil && set[ch] {
		delete(set, ch)
		close(ch)
		if len(set) == 0 {
			delete(h.subs, docID)
		}
	}
	h.refs[dir]--
	if h.refs[dir] <= 0 {
		delete(h.refs, dir)
		_ = h.w.Remove(dir)
	}
}

func (h *hub) loop() {
	for {
		select {
		case ev, ok := <-h.w.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			abs, err := filepath.Abs(ev.Name)
			if err != nil {
				continue
			}
			id := h.idFor(abs)
			h.mu.Lock()
			if len(h.subs[id]) > 0 {
				if t := h.timers[id]; t != nil {
					t.Stop()
				}
				h.timers[id] = time.AfterFunc(80*time.Millisecond, func() { h.publish(id) })
			}
			h.mu.Unlock()
		case err, ok := <-h.w.Errors:
			if !ok {
				return
			}
			log.Printf("watch error: %v", err)
		}
	}
}

func (h *hub) publish(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.timers, id)
	for ch := range h.subs[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
