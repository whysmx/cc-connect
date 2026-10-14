package wecomkf

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestStateStore_PersistsCursorAndSeenIDs(t *testing.T) {
	path := stateFilePath(t.TempDir(), "corp/1", "wk:1")
	if filepath.Base(path) != "corp_1_wk_1.json" {
		t.Fatalf("unsafe characters not sanitized: %s", path)
	}
	s, err := newStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.MarkSeen("m1") {
		t.Fatal("first MarkSeen reported duplicate")
	}
	if !s.MarkSeen("m1") {
		t.Fatal("second MarkSeen not reported as duplicate")
	}
	s.SetCursor("cur-1")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := newStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Cursor() != "cur-1" {
		t.Fatalf("cursor = %q", reloaded.Cursor())
	}
	if !reloaded.MarkSeen("m1") {
		t.Fatal("seen ID lost across restart")
	}
}

func TestStateStore_BoundedWindowAndMemoryMode(t *testing.T) {
	s, err := newStateStore("")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxSeenIDs+10; i++ {
		s.MarkSeen(fmt.Sprintf("m%d", i))
	}
	if len(s.order) != maxSeenIDs || len(s.seenSet) != maxSeenIDs {
		t.Fatalf("window not bounded: order=%d set=%d", len(s.order), len(s.seenSet))
	}
	if s.MarkSeen("m0") {
		t.Fatal("evicted ID still reported as seen")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("in-memory Save: %v", err)
	}
}

func TestStateStore_ErrorPaths(t *testing.T) {
	dir := t.TempDir()

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newStateStore(corrupt); err == nil {
		t.Fatal("corrupt state file accepted")
	}
	if _, err := newStateStore(dir); err == nil {
		t.Fatal("directory accepted as state file")
	}

	// The parent "directory" is a regular file, so Save cannot create it.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &stateStore{path: filepath.Join(blocker, "state.json"), seenSet: map[string]struct{}{}}
	s.SetCursor("c")
	if err := s.Save(); err == nil {
		t.Fatal("Save under a regular file succeeded")
	}
	if s.MarkSeen("") {
		t.Fatal("empty message ID must never count as seen")
	}
	if stateFilePath("", "c", "k") != "" {
		t.Fatal("empty data dir must mean in-memory state")
	}
}
