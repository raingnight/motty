// Alert persistence over configstore (kind "server-alerts").
//
// Storage follows the serverassets/serversched pattern: the payload is
// the Alert JSON directly, with no workbench recordPayload envelope, and
// writes use configstore.Change{Put, Expected} optimistic concurrency
// with up to three retries on conflict. Workbench's openConfig skips
// records whose kind it does not know, so these records coexist safely
// with the workbench state.
package serveralerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"motty/internal/configstore"
)

// KindAlert is the configstore kind holding one Alert per record
// (id = Alert.ID).
const KindAlert = "server-alerts"

// Writer identifies this package in configstore meta, for diagnosis only.
const writer = "serveralerts"

var (
	// ErrNotFound means no alert with that id exists.
	ErrNotFound = errors.New("serveralerts: not found")
	// ErrValidation means a field failed validation.
	ErrValidation = errors.New("serveralerts: validation failed")
	// ErrConflict is configstore's optimistic-concurrency error.
	ErrConflict = configstore.ErrConflict
)

func nowUTC() time.Time { return time.Now().UTC() }

// Store is the alert layer over a caller-supplied configstore. The
// caller owns opening the store (configstore.OpenShared) and its format;
// this package never guesses the format. Store is safe for concurrent use.
type Store struct {
	mu    sync.Mutex
	store *configstore.Store
}

// NewStore wraps store. It panics on a nil store: without a store no
// operation can succeed, and failing at construction beats failing later.
func NewStore(store *configstore.Store) *Store {
	if store == nil {
		panic("serveralerts: nil configstore.Store")
	}
	return &Store{store: store}
}

// snapshot is everything this package owns, as read in one Load, plus the
// exact stored payload bytes used as Expected guards on write.
type snapshot struct {
	alerts map[string]Alert
	raw    map[string][]byte
}

func (s *Store) load() (*snapshot, error) {
	entries, _, err := s.store.Load()
	if err != nil {
		return nil, err
	}
	snap := &snapshot{
		alerts: map[string]Alert{},
		raw:    map[string][]byte{},
	}
	for _, entry := range entries {
		if entry.Kind != KindAlert {
			continue
		}
		var alert Alert
		if err := json.Unmarshal(entry.Payload, &alert); err != nil {
			return nil, fmt.Errorf("serveralerts: corrupt alert record %s: %w", entry.ID, err)
		}
		snap.alerts[entry.ID] = alert
		snap.raw[entry.ID] = entry.Payload
	}
	return snap, nil
}

func (s *Store) commit(change configstore.Change) error {
	change.Writer = writer
	_, err := s.store.Commit(change)
	return err
}

func putEntry(alert Alert) (configstore.Entry, error) {
	payload, err := json.Marshal(alert)
	if err != nil {
		return configstore.Entry{}, err
	}
	return configstore.Entry{Kind: KindAlert, ID: alert.ID, Payload: payload}, nil
}

// List returns every stored alert, newest first (then by ID for a stable
// order when creation times tie).
func (s *Store) List() ([]Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Alert, 0, len(snap.alerts))
	for _, alert := range snap.alerts {
		out = append(out, alert)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Save upserts one alert. On a new record CreatedAt and UpdatedAt are
// both set to now (UTC); on an existing record the original CreatedAt is
// preserved and only UpdatedAt is refreshed. On an optimistic-concurrency
// conflict Save reloads and retries up to three times.
func (s *Store) Save(alert Alert) error {
	if alert.ID == "" {
		return fmt.Errorf("%w: alert id is required", ErrValidation)
	}
	if alert.ServerID == "" {
		return fmt.Errorf("%w: server id is required", ErrValidation)
	}
	if alert.Kind == "" {
		return fmt.Errorf("%w: alert kind is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := s.load()
		if err != nil {
			return err
		}
		now := nowUTC()
		if old, ok := snap.alerts[alert.ID]; ok {
			alert.CreatedAt = old.CreatedAt
		} else {
			alert.CreatedAt = now
		}
		alert.UpdatedAt = now
		entry, err := putEntry(alert)
		if err != nil {
			return err
		}
		key := configstore.Key{Kind: KindAlert, ID: alert.ID}
		err = s.commit(configstore.Change{
			Put:      []configstore.Entry{entry},
			Expected: map[configstore.Key][]byte{key: snap.raw[alert.ID]},
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("serveralerts: save alert %s: %w", alert.ID, lastErr)
}

// MarkRead marks one alert read. It only changes the Read flag (and
// UpdatedAt); marking an already-read alert is a no-op.
func (s *Store) MarkRead(id string) error {
	if id == "" {
		return fmt.Errorf("%w: alert id is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := s.load()
		if err != nil {
			return err
		}
		alert, ok := snap.alerts[id]
		if !ok {
			return ErrNotFound
		}
		if alert.Read {
			return nil
		}
		alert.Read = true
		alert.UpdatedAt = nowUTC()
		entry, err := putEntry(alert)
		if err != nil {
			return err
		}
		key := configstore.Key{Kind: KindAlert, ID: id}
		err = s.commit(configstore.Change{
			Put:      []configstore.Entry{entry},
			Expected: map[configstore.Key][]byte{key: snap.raw[id]},
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("serveralerts: mark alert %s read: %w", id, lastErr)
}

// MarkAllRead marks every unread alert read. It only changes Read flags
// (and UpdatedAt); when nothing is unread it writes nothing.
func (s *Store) MarkAllRead() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := s.load()
		if err != nil {
			return err
		}
		var puts []configstore.Entry
		expected := map[configstore.Key][]byte{}
		for id, alert := range snap.alerts {
			if alert.Read {
				continue
			}
			alert.Read = true
			alert.UpdatedAt = nowUTC()
			entry, err := putEntry(alert)
			if err != nil {
				return err
			}
			puts = append(puts, entry)
			key := configstore.Key{Kind: KindAlert, ID: id}
			expected[key] = snap.raw[id]
		}
		if len(puts) == 0 {
			return nil
		}
		err = s.commit(configstore.Change{Put: puts, Expected: expected})
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("serveralerts: mark all alerts read: %w", lastErr)
}

// PruneRecovered deletes recovered alerts whose RecoveredAt is older
// than now minus olderThan. Active alerts and recently recovered ones
// are kept. On an optimistic-concurrency conflict it reloads and
// retries up to three times.
func (s *Store) PruneRecovered(olderThan time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := s.load()
		if err != nil {
			return err
		}
		cutoff := nowUTC().Add(-olderThan)
		var deletes []configstore.Key
		expected := map[configstore.Key][]byte{}
		for id, alert := range snap.alerts {
			if alert.State != StateRecovered || alert.RecoveredAt == nil {
				continue
			}
			if !alert.RecoveredAt.Before(cutoff) {
				continue
			}
			key := configstore.Key{Kind: KindAlert, ID: id}
			deletes = append(deletes, key)
			expected[key] = snap.raw[id]
		}
		if len(deletes) == 0 {
			return nil
		}
		err = s.commit(configstore.Change{Delete: deletes, Expected: expected})
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("serveralerts: prune recovered alerts: %w", lastErr)
}
