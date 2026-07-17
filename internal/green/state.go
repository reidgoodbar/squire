package green

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const stateRelativePath = "green/state.json"

type stateStore struct {
	root string
	mu   sync.Mutex
}

func newStateStore(storeRoot string) *stateStore {
	return &stateStore{root: storeRoot}
}

func (store *stateStore) load(repoRoot string) (State, error) {
	canonical, err := canonicalRepoRoot(repoRoot)
	if err != nil {
		return State{}, err
	}
	repoRoot = canonical
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.loadLocked(repoRoot)
}

func (store *stateStore) loadLocked(repoRoot string) (State, error) {
	state := emptyState(repoRoot)
	b, err := os.ReadFile(store.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return State{}, err
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return State{}, fmt.Errorf("decode Green state: %w", err)
	}
	if state.Version != stateVersion || filepath.Clean(state.RepoRoot) != filepath.Clean(repoRoot) {
		return emptyState(repoRoot), nil
	}
	if state.Checks == nil {
		state.Checks = make(map[string]CheckRecord)
	}
	return state, nil
}

func (store *stateStore) replaceRunning(repoRoot string, pid int) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	state, err := store.loadLocked(repoRoot)
	if err != nil {
		state = emptyState(repoRoot)
	}
	for name, record := range state.Checks {
		if record.State == "running" {
			record.State = "discarded"
			record.Error = "validation process ended before publishing a result"
			record.PID = 0
			record.CompletedAt = time.Now()
			state.Checks[name] = record
		}
	}
	state.DaemonPID = pid
	state.UpdatedAt = time.Now()
	return store.saveLocked(state)
}

func (store *stateStore) update(repoRoot, configDigest string, update func(map[string]CheckRecord)) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	state, err := store.loadLocked(repoRoot)
	if err != nil {
		state = emptyState(repoRoot)
	}
	state.ConfigDigest = configDigest
	state.DaemonPID = os.Getpid()
	update(state.Checks)
	state.UpdatedAt = time.Now()
	return store.saveLocked(state)
}

func (store *stateStore) saveLocked(state State) error {
	path := store.path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf("state.%d.%d.tmp", os.Getpid(), time.Now().UnixNano()))
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (store *stateStore) path() string {
	return filepath.Join(store.root, filepath.FromSlash(stateRelativePath))
}

func emptyState(repoRoot string) State {
	return State{
		Version:  stateVersion,
		RepoRoot: filepath.Clean(repoRoot),
		Checks:   make(map[string]CheckRecord),
	}
}
