package wecomkf

import (
	"fmt"
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
