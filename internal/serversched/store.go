// Package serversched schedules agentless resource collection for managed
// servers and stores each server's latest successful metric values.
//
// Storage reuses internal/configstore (the shared config.db records table),
// following the serverassets pattern: payloads are the entity JSON directly,
// with no workbench recordPayload envelope, and writes are serialised by a
// package-level mutex plus configstore.Change{Put, Expected} optimistic
// concurrency. Two new kinds are used:
//
//   - "server-metrics": id is the server UUID, payload is a
//     servercollect.Latest. Only successful values are stored, per kind, so
//     a failed item never overwrites the previous success.
//   - "server-template": id is the template ID, payload is a
//     servercollect.Template. The built-in "default" template is virtual:
//     it is returned when no record exists and may never be deleted.
//   - "server-template-assign": id is the server UUID, payload is a
//     TemplateAssignment naming the template that server collects with.
//     No record means the default template; assigning "default" deletes
//     the record instead of storing one.
//
// Latest values are buffered in memory and flushed to disk
// periodically (see PutBuffered and Flush): a round writes to the
// buffer, which Get and List read at once, and Flush commits every
// dirty server's newest values in one Change. A crash loses values
// that were buffered but not yet flushed; after a restart those kinds
// simply read as uncollected or expired, never as zeroes.
//
// Workbench's openConfig (internal/workbench/persist_records.go, joinState)
// skips records whose kind it does not know — it leaves them out of the
// state and never takes them for deleted — so these records coexist safely
// with the workbench state, exactly like the serverassets kinds.
//
// No credential value is ever stored or returned by this package.
package serversched

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"motty/internal/configstore"
	"motty/internal/servercollect"
)

// Record kinds in config.db.
const (
	// KindMetrics holds one servercollect.Latest per server (id = server UUID).
	KindMetrics = "server-metrics"
	// KindTemplate holds one servercollect.Template per template (id = template ID).
	KindTemplate = "server-template"
	// KindTemplateAssignment holds one TemplateAssignment per server
	// (id = server UUID). No record means the default template.
	KindTemplateAssignment = "server-template-assign"
)

// DefaultTemplateID is the built-in template's ID.
const DefaultTemplateID = "default"

// Writer identifies this package in configstore meta, for diagnosis only.
const writer = "serversched"

var (
	// ErrNotFound means no record with that id exists.
	ErrNotFound = errors.New("serversched: not found")
	// ErrValidation means a field failed validation.
	ErrValidation = errors.New("serversched: validation failed")
	// ErrDefaultTemplate means the built-in default template was the target
	// of an operation that forbids it (deletion).
	ErrDefaultTemplate = errors.New("serversched: default template cannot be deleted")
	// ErrConflict is configstore's optimistic-concurrency error: a write
	// was attempted against a record that changed since it was read.
	// Callers test it with errors.Is.
	ErrConflict = configstore.ErrConflict
)

func nowUTC() time.Time { return time.Now().UTC() }

// TemplateAssignment is the persisted server -> template mapping, one
// record per server under KindTemplateAssignment. The default template
// is never stored: no record means "default".
type TemplateAssignment struct {
	ServerID   string `json:"server_id"`
	TemplateID string `json:"template_id"`
}

// Store is the latest-metrics and template layer over a caller-supplied
// configstore. The caller owns opening the store (configstore.OpenShared)
// and its format; this package never guesses the format. Store is safe for
// concurrent use.
//
// buf holds the newest buffered value per (server, kind), as written by
// PutBuffered (and mirrored by Put). Get and List overlay it on the
// stored snapshot, keeping the fresher value per kind by CollectedAt, so
// a buffered value is visible at once and a value another writer stored
// more recently is never hidden by a stale buffer entry. dirty names the
// servers whose buffered values may differ from disk; Flush writes them.
type Store struct {
	mu    sync.Mutex
	store *configstore.Store
	buf   map[string]servercollect.Latest
	dirty map[string]bool
	// commits counts Commit attempts (including ones that lost a
	// conflict); tests use it to prove a Flush wrote nothing.
	commits int
	// flushPreCommit, when set, runs between Flush building its change
	// and committing it. It is a test seam for forcing exactly one
	// optimistic-concurrency conflict; production code leaves it nil.
	flushPreCommit func()
}

// NewStore wraps store. It panics on a nil store: without a store no
// operation can succeed, and failing at construction beats failing later.
func NewStore(store *configstore.Store) *Store {
	if store == nil {
		panic("serversched: nil configstore.Store")
	}
	return &Store{
		store: store,
		buf:   map[string]servercollect.Latest{},
		dirty: map[string]bool{},
	}
}

// snapshot is everything this package owns, as read in one Load, plus the
// exact stored payload bytes used as Expected guards on write.
type snapshot struct {
	metrics    map[string]servercollect.Latest
	metricsRaw map[string][]byte
	templates  map[string]servercollect.Template
	tmplRaw    map[string][]byte
	assigns    map[string]string // server UUID -> template ID
	assignRaw  map[string][]byte
}

func (s *Store) load() (*snapshot, error) {
	entries, _, err := s.store.Load()
	if err != nil {
		return nil, err
	}
	snap := &snapshot{
		metrics:    map[string]servercollect.Latest{},
		metricsRaw: map[string][]byte{},
		templates:  map[string]servercollect.Template{},
		tmplRaw:    map[string][]byte{},
		assigns:    map[string]string{},
		assignRaw:  map[string][]byte{},
	}
	for _, entry := range entries {
		switch entry.Kind {
		case KindMetrics:
			var latest servercollect.Latest
			if err := json.Unmarshal(entry.Payload, &latest); err != nil {
				return nil, fmt.Errorf("serversched: corrupt metrics record %s: %w", entry.ID, err)
			}
			if latest.Values == nil {
				latest.Values = map[servercollect.MetricKind]servercollect.MetricValue{}
			}
			snap.metrics[entry.ID] = latest
			snap.metricsRaw[entry.ID] = entry.Payload
		case KindTemplate:
			var tmpl servercollect.Template
			if err := json.Unmarshal(entry.Payload, &tmpl); err != nil {
				return nil, fmt.Errorf("serversched: corrupt template record %s: %w", entry.ID, err)
			}
			snap.templates[entry.ID] = tmpl
			snap.tmplRaw[entry.ID] = entry.Payload
		case KindTemplateAssignment:
			var assign TemplateAssignment
			if err := json.Unmarshal(entry.Payload, &assign); err != nil {
				return nil, fmt.Errorf("serversched: corrupt template assignment %s: %w", entry.ID, err)
			}
			if strings.TrimSpace(assign.ServerID) == "" {
				assign.ServerID = entry.ID
			}
			if strings.TrimSpace(assign.TemplateID) == "" {
				return nil, fmt.Errorf("serversched: corrupt template assignment %s: empty template id", entry.ID)
			}
			snap.assigns[assign.ServerID] = assign.TemplateID
			snap.assignRaw[assign.ServerID] = entry.Payload
		}
	}
	return snap, nil
}

func putEntry(kind, id string, value any) (configstore.Entry, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return configstore.Entry{}, err
	}
	return configstore.Entry{Kind: kind, ID: id, Payload: payload}, nil
}

func (s *Store) commit(change configstore.Change) error {
	change.Writer = writer
	s.commits++
	_, err := s.store.Commit(change)
	return err
}

// applyValues merges values into dst with Put's semantics: only
// successful values (Unavailable empty, CollectedAt set) overwrite the
// previous value for their kind; failed or unavailable items leave the
// previous success in place. It reports whether any value was applied.
func applyValues(dst map[servercollect.MetricKind]servercollect.MetricValue, values []servercollect.MetricValue) bool {
	changed := false
	for _, v := range values {
		if v.Kind == "" || v.Unavailable != "" || v.CollectedAt.IsZero() {
			continue
		}
		dst[v.Kind] = v
		changed = true
	}
	return changed
}

// metricValueEqual compares two values by their JSON encoding, so a
// value that survived a disk round-trip still compares equal to the
// in-memory original (a plain DeepEqual would differ on time.Time's
// monotonic clock reading, which JSON drops).
func metricValueEqual(a, b servercollect.MetricValue) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// mergeBuffered overlays this Store's buffered values for serverID onto
// the stored Latest, keeping the fresher value per kind by CollectedAt.
// When disk holds a fresher value for a kind — another writer stored it
// after this Store buffered — the buffered copy is refreshed to disk,
// so a later read or flush never resurrects the stale value. The caller
// must hold s.mu.
func (s *Store) mergeBuffered(serverID string, disk servercollect.Latest, diskOK bool) (servercollect.Latest, bool) {
	buf, ok := s.buf[serverID]
	if !ok {
		return disk, diskOK
	}
	latest := disk
	if !diskOK {
		latest = servercollect.Latest{ServerID: serverID, Values: map[servercollect.MetricKind]servercollect.MetricValue{}}
	}
	// Copy the map: the merged view must not mutate the snapshot's
	// values, or Flush could no longer tell merged from disk.
	values := make(map[servercollect.MetricKind]servercollect.MetricValue, len(latest.Values)+len(buf.Values))
	for kind, v := range latest.Values {
		values[kind] = v
	}
	latest.Values = values
	for kind, bv := range buf.Values {
		if dv, exists := latest.Values[kind]; exists && dv.CollectedAt.After(bv.CollectedAt) {
			// Disk is fresher: adopt it into the buffer.
			buf.Values[kind] = dv
			continue
		}
		latest.Values[kind] = bv
	}
	if buf.UpdatedAt.After(latest.UpdatedAt) {
		latest.UpdatedAt = buf.UpdatedAt
	}
	return latest, true
}

// --- latest metrics ---

// Get returns one server's latest values. found is false when the server
// has never had a successful value stored. Buffered, not yet flushed
// values are visible at once.
func (s *Store) Get(serverID string) (servercollect.Latest, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return servercollect.Latest{}, false, err
	}
	latest, ok := snap.metrics[serverID]
	merged, found := s.mergeBuffered(serverID, latest, ok)
	return merged, found, nil
}

// Put merges values into the stored Latest for serverID, per kind: only
// successful values (Unavailable empty, CollectedAt set) overwrite the
// previous value for their kind; failed or unavailable items leave the
// previous success in place, to be shown as expired once StaleAfter
// passes. Put never writes a missing value as zero: kinds absent from
// values are untouched, and a call carrying no successful value writes
// nothing at all. UpdatedAt is refreshed whenever anything is stored.
// On an optimistic-concurrency conflict Put reloads and retries up to
// three times, so concurrent per-kind updates from different collection
// rounds do not lose each other's values.
func (s *Store) Put(serverID string, values []servercollect.MetricValue) error {
	if strings.TrimSpace(serverID) == "" {
		return fmt.Errorf("%w: server id is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := s.load()
		if err != nil {
			return err
		}
		latest, ok := snap.metrics[serverID]
		if !ok {
			latest = servercollect.Latest{ServerID: serverID, Values: map[servercollect.MetricKind]servercollect.MetricValue{}}
		}
		if latest.Values == nil {
			latest.Values = map[servercollect.MetricKind]servercollect.MetricValue{}
		}
		if !applyValues(latest.Values, values) {
			return nil
		}
		latest.ServerID = serverID
		latest.UpdatedAt = nowUTC()
		entry, err := putEntry(KindMetrics, serverID, latest)
		if err != nil {
			return err
		}
		key := configstore.Key{Kind: KindMetrics, ID: serverID}
		err = s.commit(configstore.Change{
			Put:      []configstore.Entry{entry},
			Expected: map[configstore.Key][]byte{key: snap.metricsRaw[serverID]},
		})
		if err == nil {
			// Mirror the write into the buffer so the buffer stays
			// the newest view this Store knows; the server stays
			// clean unless it already had unflushed values.
			s.mirrorPutLocked(serverID, values, latest.UpdatedAt)
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("serversched: put metrics for server %s: %w", serverID, lastErr)
}

// mirrorPutLocked applies a synchronous Put's values to the in-memory
// buffer, with the same per-kind merge. The caller must hold s.mu.
func (s *Store) mirrorPutLocked(serverID string, values []servercollect.MetricValue, updatedAt time.Time) {
	buf, ok := s.buf[serverID]
	if !ok {
		buf = servercollect.Latest{ServerID: serverID, Values: map[servercollect.MetricKind]servercollect.MetricValue{}}
	}
	if buf.Values == nil {
		buf.Values = map[servercollect.MetricKind]servercollect.MetricValue{}
	}
	if !applyValues(buf.Values, values) {
		return
	}
	buf.ServerID = serverID
	buf.UpdatedAt = updatedAt
	s.buf[serverID] = buf
}

// PutBuffered merges values into the in-memory buffer for serverID with
// Put's merge semantics — only successful values overwrite per kind,
// failures never overwrite, no zero values are written — and marks the
// server dirty. Get and List see the buffered values at once; nothing
// is written to disk until Flush. A call carrying no successful value
// changes nothing and marks nothing dirty.
func (s *Store) PutBuffered(serverID string, values []servercollect.MetricValue) error {
	if strings.TrimSpace(serverID) == "" {
		return fmt.Errorf("%w: server id is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	buf, ok := s.buf[serverID]
	if !ok {
		buf = servercollect.Latest{ServerID: serverID, Values: map[servercollect.MetricKind]servercollect.MetricValue{}}
	}
	if buf.Values == nil {
		buf.Values = map[servercollect.MetricKind]servercollect.MetricValue{}
	}
	if !applyValues(buf.Values, values) {
		return nil
	}
	buf.ServerID = serverID
	buf.UpdatedAt = nowUTC()
	s.buf[serverID] = buf
	s.dirty[serverID] = true
	return nil
}

// Flush writes every dirty server's buffered latest values to disk in a
// single Load plus a single Commit, merging the buffered kinds over the
// freshly loaded snapshot so kinds another writer updated meanwhile are
// kept. With no dirty server it commits nothing and does not even load.
// On an optimistic-concurrency conflict it reloads and retries up to
// three times, like Put. A failed Flush keeps the dirty marks, so the
// next Flush retries the same values and nothing is lost; only a process
// crash loses unflushed values, and those then read as uncollected or
// expired after a restart, never as zeroes.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dirty) == 0 {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := s.load()
		if err != nil {
			return err
		}
		change := configstore.Change{Expected: map[configstore.Key][]byte{}}
		dirty := make([]string, 0, len(s.dirty))
		for id := range s.dirty {
			dirty = append(dirty, id)
		}
		sort.Strings(dirty)
		for _, id := range dirty {
			disk, diskOK := snap.metrics[id]
			merged, _ := s.mergeBuffered(id, disk, diskOK)
			if diskOK {
				changed := false
				for kind, v := range merged.Values {
					if cur, exists := disk.Values[kind]; !exists || !metricValueEqual(cur, v) {
						changed = true
						break
					}
				}
				if !changed {
					continue
				}
			}
			merged.ServerID = id
			if merged.UpdatedAt.IsZero() {
				merged.UpdatedAt = nowUTC()
			}
			entry, err := putEntry(KindMetrics, id, merged)
			if err != nil {
				return err
			}
			key := configstore.Key{Kind: KindMetrics, ID: id}
			change.Put = append(change.Put, entry)
			change.Expected[key] = snap.metricsRaw[id]
		}
		if len(change.Put) == 0 {
			// Everything buffered is already on disk (a synchronous
			// Put wrote the same values): nothing to commit.
			s.dirty = map[string]bool{}
			return nil
		}
		if s.flushPreCommit != nil {
			s.flushPreCommit()
		}
		if err := s.commit(change); err == nil {
			s.dirty = map[string]bool{}
			return nil
		} else if !errors.Is(err, ErrConflict) {
			return err
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("serversched: flush metrics: %w", lastErr)
}

// List returns every server's latest values, sorted by server ID, for
// home-page statistics. Servers with no stored values do not appear.
// Buffered, not yet flushed values are included.
func (s *Store) List() ([]servercollect.Latest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return nil, err
	}
	merged := map[string]servercollect.Latest{}
	for id, latest := range snap.metrics {
		merged[id], _ = s.mergeBuffered(id, latest, true)
	}
	for id := range s.buf {
		if _, ok := merged[id]; !ok {
			merged[id], _ = s.mergeBuffered(id, servercollect.Latest{}, false)
		}
	}
	out := make([]servercollect.Latest, 0, len(merged))
	for _, latest := range merged {
		out = append(out, latest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
	return out, nil
}

// Delete removes a server's latest values, for use when its asset is
// soft-deleted, and drops its template assignment and any buffered
// values, so a later Flush cannot resurrect them. Deleting a server
// with no stored values is a no-op.
func (s *Store) Delete(serverID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return err
	}
	change := configstore.Change{Expected: map[configstore.Key][]byte{}}
	if raw, ok := snap.metricsRaw[serverID]; ok {
		key := configstore.Key{Kind: KindMetrics, ID: serverID}
		change.Delete = append(change.Delete, key)
		change.Expected[key] = raw
	}
	if raw, ok := snap.assignRaw[serverID]; ok {
		key := configstore.Key{Kind: KindTemplateAssignment, ID: serverID}
		change.Delete = append(change.Delete, key)
		change.Expected[key] = raw
	}
	if len(change.Delete) == 0 {
		delete(s.buf, serverID)
		delete(s.dirty, serverID)
		return nil
	}
	if err := s.commit(change); err != nil {
		return err
	}
	delete(s.buf, serverID)
	delete(s.dirty, serverID)
	return nil
}

// --- templates ---

// GetTemplate returns one template by ID. The built-in default template is
// returned (with found=true) even when no record for it exists.
func (s *Store) GetTemplate(id string) (servercollect.Template, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return servercollect.Template{}, false, nil
	}
	snap, err := s.load()
	if err != nil {
		return servercollect.Template{}, false, err
	}
	if tmpl, ok := snap.templates[id]; ok {
		return tmpl, true, nil
	}
	if id == DefaultTemplateID {
		return servercollect.DefaultTemplate(), true, nil
	}
	return servercollect.Template{}, false, nil
}

// ListTemplates returns all stored templates plus the built-in default
// when it has no record, default first, then the rest sorted by ID.
func (s *Store) ListTemplates() ([]servercollect.Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return nil, err
	}
	out := []servercollect.Template{}
	if _, ok := snap.templates[DefaultTemplateID]; !ok {
		out = append(out, servercollect.DefaultTemplate())
	}
	ids := make([]string, 0, len(snap.templates))
	for id := range snap.templates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, snap.templates[id])
	}
	return out, nil
}

// SaveTemplate stores a template, creating it when new. The ID must be
// set; the name defaults to the ID when empty.
func (s *Store) SaveTemplate(tmpl servercollect.Template) error {
	tmpl.ID = strings.TrimSpace(tmpl.ID)
	if tmpl.ID == "" {
		return fmt.Errorf("%w: template id is required", ErrValidation)
	}
	if strings.TrimSpace(tmpl.Name) == "" {
		tmpl.Name = tmpl.ID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return err
	}
	entry, err := putEntry(KindTemplate, tmpl.ID, tmpl)
	if err != nil {
		return err
	}
	key := configstore.Key{Kind: KindTemplate, ID: tmpl.ID}
	return s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: snap.tmplRaw[tmpl.ID]},
	})
}

// DeleteTemplate removes a template. The built-in default template may
// never be deleted; deleting a template that does not exist is a no-op.
func (s *Store) DeleteTemplate(id string) error {
	id = strings.TrimSpace(id)
	if id == DefaultTemplateID {
		return ErrDefaultTemplate
	}
	if id == "" {
		return fmt.Errorf("%w: template id is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return err
	}
	raw, ok := snap.tmplRaw[id]
	if !ok {
		return nil
	}
	key := configstore.Key{Kind: KindTemplate, ID: id}
	return s.commit(configstore.Change{
		Delete:   []configstore.Key{key},
		Expected: map[configstore.Key][]byte{key: raw},
	})
}

// --- template assignments ---

// SaveTemplateAssignment records which template serverID collects with.
// Assigning the default template deletes the record instead of storing
// one: no record means "default", so the default assignment never
// leaves a stale record behind.
func (s *Store) SaveTemplateAssignment(serverID, templateID string) error {
	serverID = strings.TrimSpace(serverID)
	templateID = strings.TrimSpace(templateID)
	if serverID == "" {
		return fmt.Errorf("%w: server id is required", ErrValidation)
	}
	if templateID == "" {
		return fmt.Errorf("%w: template id is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return err
	}
	key := configstore.Key{Kind: KindTemplateAssignment, ID: serverID}
	if templateID == DefaultTemplateID {
		if _, ok := snap.assignRaw[serverID]; !ok {
			return nil
		}
		return s.commit(configstore.Change{
			Delete:   []configstore.Key{key},
			Expected: map[configstore.Key][]byte{key: snap.assignRaw[serverID]},
		})
	}
	entry, err := putEntry(KindTemplateAssignment, serverID, TemplateAssignment{ServerID: serverID, TemplateID: templateID})
	if err != nil {
		return err
	}
	return s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: snap.assignRaw[serverID]},
	})
}

// ListTemplateAssignments returns every stored server -> template
// assignment. Servers without a record collect with the default
// template and do not appear here.
func (s *Store) ListTemplateAssignments() (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(snap.assigns))
	for serverID, templateID := range snap.assigns {
		out[serverID] = templateID
	}
	return out, nil
}

// DeleteTemplateAssignment removes one server's template assignment, so
// it collects with the default template again. Deleting an assignment
// that does not exist is a no-op.
func (s *Store) DeleteTemplateAssignment(serverID string) error {
	serverID = strings.TrimSpace(serverID)
	if serverID == "" {
		return fmt.Errorf("%w: server id is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return err
	}
	raw, ok := snap.assignRaw[serverID]
	if !ok {
		return nil
	}
	key := configstore.Key{Kind: KindTemplateAssignment, ID: serverID}
	return s.commit(configstore.Change{
		Delete:   []configstore.Key{key},
		Expected: map[configstore.Key][]byte{key: raw},
	})
}
