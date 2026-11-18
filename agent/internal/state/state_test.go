package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return NewStore(filepath.Join(dir, "state.json"), filepath.Join(dir, "deploy.lock"))
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	st, err := newStore(t).Load()
	if err != nil || st.Current != nil {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in := State{Current: &Release{Version: "abc", Image: "repo:abc", Color: "blue", DeployedAt: at}}
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	out, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if out.Current.Version != "abc" || !out.Current.DeployedAt.Equal(at) {
		t.Fatalf("out = %+v", out.Current)
	}
}

// deploy.sh writes this exact shape with jq; the agent must keep parsing it.
func TestLoadParsesDeployScriptFormat(t *testing.T) {
	s := newStore(t)
	raw := `{"current":{"version":"8f3a2b1","image":"u/parkd:8f3a2b1","color":"green","deployed_at":"2026-11-18T10:00:00Z"},
	"previous":{"version":"7c1d9e0","image":"u/parkd:7c1d9e0","color":"blue","deployed_at":"2026-11-17T10:00:00Z"},
	"quarantined":[],"history":[{"at":"2026-11-18T10:00:00Z","kind":"deploy","version":"8f3a2b1","from":"7c1d9e0"}]}`
	if err := os.WriteFile(s.path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Current.Color != "green" || st.Previous.Version != "7c1d9e0" || len(st.History) != 1 {
		t.Fatalf("st = %+v", st)
	}
}

func TestLoadRejectsCorruptFile(t *testing.T) {
	s := newStore(t)
	if err := os.WriteFile(s.path, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestRecordRollback(t *testing.T) {
	st := State{
		Current:  &Release{Version: "bad", Color: "green"},
		Previous: &Release{Version: "good", Color: "blue"},
	}
	st.RecordRollback(time.Now(), "oom_killed")
	if st.Current.Version != "good" || st.Previous != nil {
		t.Fatalf("st = %+v", st)
	}
	if len(st.Quarantined) != 1 || st.Quarantined[0] != "bad" {
		t.Fatalf("quarantined = %v", st.Quarantined)
	}
	if got := st.History[0]; got.Kind != "rollback" || got.From != "bad" || got.Version != "good" {
		t.Fatalf("history = %+v", got)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	st := State{History: make([]Event, maxHistory)}
	st.Current, st.Previous = &Release{Version: "bad"}, &Release{Version: "good"}
	st.RecordRollback(time.Now(), "x")
	if len(st.History) != maxHistory {
		t.Fatalf("len = %d", len(st.History))
	}
}

func TestLockExcludesSecondHolder(t *testing.T) {
	s := newStore(t)
	unlock, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := s.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock err = %v", err)
	}

	unlock()
	again, err := s.Lock(context.Background())
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}

func TestWriteFileAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "active.conf")
	if err := WriteFileAtomic(path, []byte("server a;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("perm = %v", info.Mode().Perm())
	}
}
