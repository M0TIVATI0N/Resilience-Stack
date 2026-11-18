// Package state reads and writes the release state shared with deploy.sh.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

const (
	maxHistory       = 50
	lockPollInterval = 100 * time.Millisecond
)

// Release is one deployed version of the application.
type Release struct {
	Version    string    `json:"version"`
	Image      string    `json:"image"`
	Color      string    `json:"color"`
	DeployedAt time.Time `json:"deployed_at"`
}

// Event is one entry of the deploy and rollback journal.
type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Version string    `json:"version"`
	From    string    `json:"from,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// State describes what is live and what can be rolled back to.
type State struct {
	Current     *Release `json:"current"`
	Previous    *Release `json:"previous"`
	Quarantined []string `json:"quarantined"`
	History     []Event  `json:"history"`
}

// RecordRollback promotes the previous release and quarantines the bad one.
func (s *State) RecordRollback(at time.Time, reason string) {
	bad, good := s.Current, s.Previous
	s.Current, s.Previous = good, nil
	if !slices.Contains(s.Quarantined, bad.Version) {
		s.Quarantined = append(s.Quarantined, bad.Version)
	}
	s.History = append(s.History, Event{At: at, Kind: "rollback", Version: good.Version, From: bad.Version, Reason: reason})
	if extra := len(s.History) - maxHistory; extra > 0 {
		s.History = s.History[extra:]
	}
}

// Store persists State in a JSON file.
type Store struct {
	path     string
	lockPath string
}

// NewStore creates a store for the given state and lock files.
func NewStore(path, lockPath string) *Store {
	return &Store{path: path, lockPath: lockPath}
}

// Load returns the saved state, or an empty state before the first deploy.
func (s *Store) Load() (State, error) {
	var st State
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("parse state: %w", err)
	}
	return st, nil
}

// Save atomically replaces the state file.
func (s *Store) Save(st State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	return WriteFileAtomic(s.path, append(raw, '\n'), 0o664)
}

// Lock takes the exclusive deploy lock, the same flock(2) that deploy.sh uses.
// It waits until the lock is free or ctx ends.
func (s *Store) Lock(ctx context.Context) (unlock func(), err error) {
	file, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o664)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, fmt.Errorf("flock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("wait for deploy lock: %w", ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
}

// WriteFileAtomic writes via a temporary file and rename, so readers never see
// a partially written file.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
