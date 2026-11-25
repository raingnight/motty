package serverbatch

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"motty/internal/configstore"
)

// Command templates.
//
// Storage reuses internal/configstore (the shared config.db records
// table), following the serverassets/serveralerts pattern: the payload is
// the Template JSON directly, with no workbench recordPayload envelope,
// and writes use configstore.Change{Put, Expected} optimistic
// concurrency. Kind is "server-batch-template", id is the template UUID.
// Only user templates are stored; the built-in templates from
// BuiltinTemplates are virtual and never written.

const (
	// KindTemplate holds one user Template per record (id = template UUID).
	KindTemplate = "server-batch-template"
)

const writer = "serverbatch"

var (
	// ErrConflict is configstore's optimistic-concurrency error.
	ErrConflict = configstore.ErrConflict
)

// Template is a saved batch command. OS is empty for a generic template,
// otherwise a distro name such as "ubuntu" or "centos".
type Template struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Command     string    `json:"command"`
	Description string    `json:"description,omitempty"`
	OS          string    `json:"os,omitempty"`
	Risk        RiskLevel `json:"risk"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// BuiltinTemplates returns the four built-in templates. They are virtual:
// callers show them alongside stored templates but never persist them,
// and TemplateStore rejects them (their IDs are reserved).
func BuiltinTemplates() []Template {
	now := nowUTC()
	mk := func(id, name, command, desc, os string) Template {
		return Template{
			ID: id, Name: name, Command: command, Description: desc, OS: os,
			Risk: ClassifyRisk(command).Level, CreatedAt: now, UpdatedAt: now,
		}
	}
	return []Template{
		mk("builtin-system-update", "系统更新",
			"sudo apt-get update && sudo apt-get upgrade -y",
			"更新系统软件包（Ubuntu/Debian）", "ubuntu"),
		mk("builtin-service-restart", "服务重启",
			"sudo systemctl restart nginx",
			"重启 nginx 服务（请按实际服务名修改）", ""),
		mk("builtin-log-view", "日志查看",
			"journalctl -u sshd --since '1 hour ago' -n 100",
			"查看 sshd 最近一小时的日志", ""),
		mk("builtin-disk-cleanup", "磁盘清理",
			"sudo journalctl --vacuum-size=100M",
			"清理系统日志至 100M 以内", ""),
	}
}

func isBuiltinID(id string) bool {
	return strings.HasPrefix(id, "builtin-")
}

// TemplateStore is the user-template layer over a caller-supplied
// configstore. The caller owns opening the store (configstore.OpenShared)
// and its format; this package never guesses the format. Store is safe
// for concurrent use.
type TemplateStore struct {
	mu    sync.Mutex
	store *configstore.Store
}

// NewTemplateStore wraps store. It panics on a nil store: without a store
// no operation can succeed, and failing at construction beats failing
// later.
func NewTemplateStore(store *configstore.Store) *TemplateStore {
	if store == nil {
		panic("serverbatch: nil configstore.Store")
	}
	return &TemplateStore{store: store}
}

type templateSnapshot struct {
	templates map[string]Template
	raw       map[string][]byte
}

func (s *TemplateStore) load() (*templateSnapshot, error) {
	entries, _, err := s.store.Load()
	if err != nil {
		return nil, err
	}
	snap := &templateSnapshot{
		templates: map[string]Template{},
		raw:       map[string][]byte{},
	}
	for _, entry := range entries {
		if entry.Kind != KindTemplate {
			continue
		}
		var tpl Template
		if err := json.Unmarshal(entry.Payload, &tpl); err != nil {
			return nil, fmt.Errorf("serverbatch: corrupt template record %s: %w", entry.ID, err)
		}
		snap.templates[entry.ID] = tpl
		snap.raw[entry.ID] = entry.Payload
	}
	return snap, nil
}

func (s *TemplateStore) commit(change configstore.Change) error {
	change.Writer = writer
	_, err := s.store.Commit(change)
	return err
}

func putTemplateEntry(tpl Template) (configstore.Entry, error) {
	payload, err := json.Marshal(tpl)
	if err != nil {
		return configstore.Entry{}, err
	}
	return configstore.Entry{Kind: KindTemplate, ID: tpl.ID, Payload: payload}, nil
}

func validateTemplate(tpl *Template) error {
	tpl.Name = strings.TrimSpace(tpl.Name)
	if tpl.Name == "" {
		return fmt.Errorf("%w: template name is required", ErrValidation)
	}
	tpl.Command = strings.TrimSpace(tpl.Command)
	if tpl.Command == "" {
		return fmt.Errorf("%w: template command is required", ErrValidation)
	}
	tpl.Description = strings.TrimSpace(tpl.Description)
	tpl.OS = strings.TrimSpace(tpl.OS)
	// Risk is always re-derived from the command, never trusted from
	// the caller: the stored grade matches ClassifyRisk.
	tpl.Risk = ClassifyRisk(tpl.Command).Level
	return nil
}

// Create stores a new user template, generating its UUID. The Risk is
// re-derived from the command via ClassifyRisk.
func (s *TemplateStore) Create(t Template) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateTemplate(&t); err != nil {
		return Template{}, err
	}
	t.ID = newUUID()
	now := nowUTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	entry, err := putTemplateEntry(t)
	if err != nil {
		return Template{}, err
	}
	key := configstore.Key{Kind: KindTemplate, ID: t.ID}
	if err := s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: nil},
	}); err != nil {
		return Template{}, err
	}
	return t, nil
}

// Update replaces a user template's fields; ID and CreatedAt are
// immutable. Built-in templates are virtual and cannot be updated.
func (s *TemplateStore) Update(t Template) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(t.ID) == "" || isBuiltinID(t.ID) {
		return ErrNotFound
	}
	snap, err := s.load()
	if err != nil {
		return err
	}
	old, ok := snap.templates[t.ID]
	if !ok {
		return ErrNotFound
	}
	if err := validateTemplate(&t); err != nil {
		return err
	}
	t.ID = old.ID
	t.CreatedAt = old.CreatedAt
	t.UpdatedAt = nowUTC()
	entry, err := putTemplateEntry(t)
	if err != nil {
		return err
	}
	key := configstore.Key{Kind: KindTemplate, ID: t.ID}
	return s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: snap.raw[t.ID]},
	})
}

// Delete removes a user template. Deleting a missing template is a
// no-op; built-in templates cannot be deleted.
func (s *TemplateStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if isBuiltinID(id) {
		return ErrNotFound
	}
	snap, err := s.load()
	if err != nil {
		return err
	}
	raw, ok := snap.raw[id]
	if !ok {
		return nil
	}
	return s.commit(configstore.Change{
		Delete:   []configstore.Key{{Kind: KindTemplate, ID: id}},
		Expected: map[configstore.Key][]byte{{Kind: KindTemplate, ID: id}: raw},
	})
}

// Get returns one stored user template. Built-in templates are not
// stored: use BuiltinTemplates for those.
func (s *TemplateStore) Get(id string) (Template, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return Template{}, false
	}
	tpl, ok := snap.templates[id]
	return tpl, ok
}

// List returns all stored user templates, sorted by name
// (case-insensitive, stable; ID breaks ties).
func (s *TemplateStore) List() []Template {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return nil
	}
	out := make([]Template, 0, len(snap.templates))
	for _, tpl := range snap.templates {
		out = append(out, tpl)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name); a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}
