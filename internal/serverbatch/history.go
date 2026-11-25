package serverbatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"motty/internal/configstore"
)

// Run history (§13.2, §10.4).
//
// Storage reuses internal/configstore (the shared config.db records
// table), following the serverassets/serveralerts pattern: the payload is
// the Run JSON directly, with no workbench recordPayload envelope, and
// writes use configstore.Change{Put, Expected} optimistic concurrency
// with up to three retries on conflict. Kind is "server-batch-history",
// id is the Run ID; one record holds one whole Run, including every
// server's output, exit code, duration and retry count.
//
// Retention is 30 days: Prune deletes runs whose FinishedAt is more
// than 30 days old. A run whose FinishedAt is exactly 30 days old is
// kept; one second older is deleted. A run that never finished
// (FinishedAt zero) is never pruned by time alone.
//
// Note the configstore payload cap (MaxPayload, 1<<20 bytes): a Run with
// many servers or very large retained outputs can exceed one record.
// Per-server output is already truncated to MaxOutputBytes by the
// executor, but a caller saving a Run with dozens of near-limit outputs
// will get a payload-too-large error from SaveRun instead of a silent
// truncation; shrinking MaxOutputBytes is the remedy.

const (
	// KindHistory holds one Run per record (id = Run ID).
	KindHistory = "server-batch-history"
	// HistoryRetention is how long runs are kept.
	HistoryRetention = 30 * 24 * time.Hour
)

// HistoryFilter selects runs for ListRuns. Zero values match everything:
// an empty ServerID matches all servers, a zero From/To leaves that end
// open, and HasState=false ignores State. Limit <= 0 means no limit.
type HistoryFilter struct {
	ServerID string
	From, To time.Time
	State    ResultState
	HasState bool
	Limit    int
}

// HistoryStore is the run-history layer over a caller-supplied
// configstore. The caller owns opening the store (configstore.OpenShared)
// and its format; this package never guesses the format. Store is safe
// for concurrent use.
type HistoryStore struct {
	mu    sync.Mutex
	store *configstore.Store
}

// NewHistoryStore wraps store. It panics on a nil store: without a store
// no operation can succeed, and failing at construction beats failing
// later.
func NewHistoryStore(store *configstore.Store) *HistoryStore {
	if store == nil {
		panic("serverbatch: nil configstore.Store")
	}
	return &HistoryStore{store: store}
}

type historySnapshot struct {
	runs map[string]Run
	raw  map[string][]byte
}

func (s *HistoryStore) load() (*historySnapshot, error) {
	entries, _, err := s.store.Load()
	if err != nil {
		return nil, err
	}
	snap := &historySnapshot{
		runs: map[string]Run{},
		raw:  map[string][]byte{},
	}
	for _, entry := range entries {
		if entry.Kind != KindHistory {
			continue
		}
		var run Run
		if err := json.Unmarshal(entry.Payload, &run); err != nil {
			return nil, fmt.Errorf("serverbatch: corrupt history record %s: %w", entry.ID, err)
		}
		snap.runs[entry.ID] = run
		snap.raw[entry.ID] = entry.Payload
	}
	return snap, nil
}

func (s *HistoryStore) commit(change configstore.Change) error {
	change.Writer = writer
	_, err := s.store.Commit(change)
	return err
}

func putRunEntry(run Run) (configstore.Entry, error) {
	payload, err := json.Marshal(run)
	if err != nil {
		return configstore.Entry{}, err
	}
	return configstore.Entry{Kind: KindHistory, ID: run.ID, Payload: payload}, nil
}

// SaveRun upserts one run. On an optimistic-concurrency conflict it
// reloads and retries up to three times. The Run is stored as given:
// callers should save the finished Run, not the in-progress one.
func (s *HistoryStore) SaveRun(r *Run) error {
	if r == nil {
		return fmt.Errorf("%w: run is required", ErrValidation)
	}
	if r.ID == "" {
		return fmt.Errorf("%w: run id is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := s.load()
		if err != nil {
			return err
		}
		entry, err := putRunEntry(*r)
		if err != nil {
			return err
		}
		key := configstore.Key{Kind: KindHistory, ID: r.ID}
		err = s.commit(configstore.Change{
			Put:      []configstore.Entry{entry},
			Expected: map[configstore.Key][]byte{key: snap.raw[r.ID]},
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("serverbatch: save run %s: %w", r.ID, lastErr)
}

// GetRun returns one run by ID.
func (s *HistoryStore) GetRun(id string) (*Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return nil, false
	}
	run, ok := snap.runs[id]
	if !ok {
		return nil, false
	}
	copy := run
	return &copy, true
}

// ListRuns returns runs matching f, newest first (by CreatedAt, then by
// ID for a stable order when creation times tie). From/To filter on
// CreatedAt; State matches a run that has at least one target in that
// state; ServerID matches a run that targeted that server.
func (s *HistoryStore) ListRuns(f HistoryFilter) []*Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return nil
	}
	out := []*Run{}
	for _, run := range snap.runs {
		if !f.From.IsZero() && run.CreatedAt.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && run.CreatedAt.After(f.To) {
			continue
		}
		if f.ServerID != "" || f.HasState {
			matched := false
			for _, target := range run.Targets {
				if f.ServerID != "" && target.ServerID != f.ServerID {
					continue
				}
				if f.HasState && target.State != f.State {
					continue
				}
				matched = true
				break
			}
			if !matched {
				continue
			}
		}
		copy := run
		out = append(out, &copy)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// DeleteRun removes one run. Deleting a missing run is a no-op.
func (s *HistoryStore) DeleteRun(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return err
	}
	raw, ok := snap.raw[id]
	if !ok {
		return nil
	}
	return s.commit(configstore.Change{
		Delete:   []configstore.Key{{Kind: KindHistory, ID: id}},
		Expected: map[configstore.Key][]byte{{Kind: KindHistory, ID: id}: raw},
	})
}

// Prune deletes runs whose FinishedAt is more than HistoryRetention old
// and returns how many were deleted. A run finished exactly
// HistoryRetention ago is kept; a run that never finished is kept.
// On an optimistic-concurrency conflict it reloads and retries up to
// three times.
func (s *HistoryStore) Prune() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := s.load()
		if err != nil {
			return 0, err
		}
		cutoff := nowUTC().Add(-HistoryRetention)
		var deletes []configstore.Key
		expected := map[configstore.Key][]byte{}
		for id, run := range snap.runs {
			if run.FinishedAt.IsZero() || !run.FinishedAt.Before(cutoff) {
				continue
			}
			key := configstore.Key{Kind: KindHistory, ID: id}
			deletes = append(deletes, key)
			expected[key] = snap.raw[id]
		}
		if len(deletes) == 0 {
			return 0, nil
		}
		err = s.commit(configstore.Change{Delete: deletes, Expected: expected})
		if err == nil {
			return len(deletes), nil
		}
		if !errors.Is(err, ErrConflict) {
			return 0, err
		}
		lastErr = err
	}
	return 0, fmt.Errorf("serverbatch: prune history: %w", lastErr)
}
