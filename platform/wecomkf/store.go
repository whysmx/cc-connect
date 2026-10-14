package wecomkf

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// maxSeenIDs bounds the persisted message-ID dedup window. sync_msg only
// returns the last 3 days of messages, and the cursor already prevents
// re-reading old pages, so a few thousand IDs are plenty.
const maxSeenIDs = 4096

// syncState is the persisted pull state of one customer-service account.
type syncState struct {
	Cursor string   `json:"cursor"`
	Seen   []string `json:"seen,omitempty"`
}

// stateStore persists the kf/sync_msg cursor and recently processed message
// IDs under <data_dir>/wecom_kf/, so a restart neither replays nor re-answers
// messages. With an empty path it keeps state in memory only.
type stateStore struct {
	path string

	mu      sync.Mutex
	cursor  string
	order   []string
	seenSet map[string]struct{}
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// stateFilePath returns the state file for a corp/account pair, or "" when
// dataDir is empty.
func stateFilePath(dataDir, corpID, openKfID string) string {
	if dataDir == "" {
		return ""
	}
	name := unsafeFileChars.ReplaceAllString(corpID, "_") + "_" + unsafeFileChars.ReplaceAllString(openKfID, "_") + ".json"
	return filepath.Join(dataDir, "wecom_kf", name)
}

func newStateStore(path string) (*stateStore, error) {
	s := &stateStore{path: path, seenSet: make(map[string]struct{})}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("wecom_kf: read state %s: %w", path, err)
	}
	var st syncState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("wecom_kf: parse state %s: %w", path, err)
	}
	s.cursor = st.Cursor
	for _, id := range st.Seen {
		s.markLocked(id)
	}
	return s, nil
}

func (s *stateStore) Cursor() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

func (s *stateStore) SetCursor(cursor string) {
	s.mu.Lock()
	s.cursor = cursor
	s.mu.Unlock()
}

// MarkSeen records msgID and reports whether it had already been seen.
func (s *stateStore) MarkSeen(msgID string) (alreadySeen bool) {
	if msgID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seenSet[msgID]; ok {
		return true
	}
	s.markLocked(msgID)
	return false
}

func (s *stateStore) markLocked(id string) {
	if _, ok := s.seenSet[id]; ok {
		return
	}
	s.seenSet[id] = struct{}{}
	s.order = append(s.order, id)
	for len(s.order) > maxSeenIDs {
		delete(s.seenSet, s.order[0])
		s.order = s.order[1:]
	}
}

// Save atomically writes the state file (no-op for in-memory stores).
func (s *stateStore) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	st := syncState{Cursor: s.cursor, Seen: append([]string(nil), s.order...)}
	s.mu.Unlock()

	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("wecom_kf: encode state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("wecom_kf: create state dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("wecom_kf: write state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("wecom_kf: replace state: %w", err)
	}
	return nil
}
