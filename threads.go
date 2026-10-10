package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// threadStore remembers the threads this bot has spoken in, so a
// follow-up there gets an answer without another mention. It survives
// restarts in the extension's data directory.
type threadStore struct {
	mu   sync.Mutex
	path string
	seen map[string]int64
}

const maxTrackedThreads = 2000

func newThreadStore(dataDir string) *threadStore {
	ts := &threadStore{seen: map[string]int64{}}
	if dataDir == "" {
		return ts
	}
	ts.path = filepath.Join(dataDir, "threads.json")
	if data, err := os.ReadFile(ts.path); err == nil {
		_ = json.Unmarshal(data, &ts.seen)
		if ts.seen == nil {
			ts.seen = map[string]int64{}
		}
	}
	return ts
}

func (t *threadStore) has(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.seen[id]
	return ok
}

func (t *threadStore) add(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, known := t.seen[id]
	t.seen[id] = time.Now().Unix()
	if known {
		// Only the timestamp moved; not worth a write per message.
		return
	}
	if len(t.seen) > maxTrackedThreads {
		ids := make([]string, 0, len(t.seen))
		for k := range t.seen {
			ids = append(ids, k)
		}
		sort.Slice(ids, func(i, j int) bool { return t.seen[ids[i]] < t.seen[ids[j]] })
		for _, k := range ids[:len(ids)-maxTrackedThreads] {
			delete(t.seen, k)
		}
	}
	if t.path == "" {
		return
	}
	data, err := json.Marshal(t.seen)
	if err != nil {
		return
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, t.path)
	}
}
