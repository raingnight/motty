// Package serverassets stores server assets (servers, groups, jump hosts)
// for centralised Linux server management.
//
// Storage reuses internal/configstore: one generic records table
// (kind, id, stamp, deleted, payload JSON). This package uses its own kinds
// ("server", "server-group", "jump-host") and stores the payload as the
// entity JSON directly, without the workbench recordPayload envelope, and
// does not participate in workbench entityFields synchronisation (known gap:
// second-stage assets are not synced between devices).
//
// Soft deletion is payload-level (Server.Deleted), not a configstore
// tombstone, so a deleted server keeps its payload and can be restored.
// Credentials are never stored here: only a reference string naming a vault
// record (see credential.go). No credential value ever appears in a log or
// an error returned by this package.
package serverassets

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"motty/internal/configstore"
)

// Record kinds in config.db. Workbench's openConfig skips unknown kinds and
// never deletes them, so these records coexist safely with existing data.
const (
	KindServer    = "server"
	KindServerGrp = "server-group"
	KindJumpHost  = "jump-host"
)

// Writer identifies this package in configstore meta, for diagnosis only.
const writer = "serverassets"

var (
	// ErrValidation means a field failed validation; the message names the
	// field, never a secret value.
	ErrValidation = errors.New("serverassets: validation failed")
	// ErrDuplicate means a live server already uses the same address+port.
	ErrDuplicate = errors.New("serverassets: duplicate server")
	// ErrNotFound means no record with that id exists.
	ErrNotFound = errors.New("serverassets: not found")
	// ErrJumpHostInUse means a jump host is still referenced by a server.
	ErrJumpHostInUse = errors.New("serverassets: jump host is still referenced by a server")
)

// Auth methods.
const (
	AuthPassword = "password"
	AuthKey      = "key"
)

// Sudo modes. SudoCommands is only a reservation for a later stage.
const (
	SudoNone     = "none"
	SudoNoPasswd = "nopasswd"
)

// CheckStatus values for Filter.Status.
const (
	StatusUnknown = "unknown"
	StatusOK      = "ok"
	StatusFailed  = "failed"
)

// CheckResult is the outcome of the latest connectivity check of a server.
type CheckResult struct {
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	Stage string    `json:"stage,omitempty"`
	Error string    `json:"error,omitempty"`
}

// Server is one managed Linux server. ID is a system-generated UUID,
// permanent for the life of the asset; IP/hostname may change.
type Server struct {
	ID                 string       `json:"id"`
	Name               string       `json:"name"`
	Hostname           string       `json:"hostname,omitempty"`
	Address            string       `json:"address"`
	Port               int          `json:"port"`
	User               string       `json:"user"`
	AuthMethod         string       `json:"auth_method"`
	IdentityFile       string       `json:"identity_file,omitempty"`
	CredentialRef      string       `json:"credential_ref,omitempty"`
	JumpHostID         string       `json:"jump_host_id,omitempty"`
	GroupID            string       `json:"group_id,omitempty"`
	Tags               []string     `json:"tags,omitempty"`
	Notes              string       `json:"notes,omitempty"`
	OS                 string       `json:"os,omitempty"`
	HostKeyFingerprint string       `json:"host_key_fingerprint,omitempty"`
	SudoMode           string       `json:"sudo_mode,omitempty"`
	SudoCommands       []string     `json:"sudo_commands,omitempty"`
	Deleted            bool         `json:"deleted,omitempty"`
	DeletedAt          *time.Time   `json:"deleted_at,omitempty"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
	LastCheck          *CheckResult `json:"last_check,omitempty"`
}

// ServerGroup groups servers. Names are unique (case-insensitive).
type ServerGroup struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// JumpHost is a single-hop SSH bastion shared by servers that reference it.
type JumpHost struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Address            string    `json:"address"`
	Port               int       `json:"port"`
	User               string    `json:"user"`
	AuthMethod         string    `json:"auth_method"`
	IdentityFile       string    `json:"identity_file,omitempty"`
	CredentialRef      string    `json:"credential_ref,omitempty"`
	HostKeyFingerprint string    `json:"host_key_fingerprint,omitempty"`
	Notes              string    `json:"notes,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// Filter selects servers for ListServers. Empty fields match everything.
// Status is one of StatusUnknown/StatusOK/StatusFailed (case-insensitive).
// Query is a case-insensitive substring match on name, hostname or address.
type Filter struct {
	GroupID        string
	Tag            string
	OS             string
	Status         string
	Query          string
	IncludeDeleted bool
}

// Store is the server-asset layer over a caller-supplied configstore.
// The caller owns opening the store (configstore.OpenShared) and its format;
// this package never guesses the format. Store is safe for concurrent use.
type Store struct {
	mu    sync.Mutex
	store *configstore.Store
}

// NewStore wraps store. It panics on a nil store: without a store no
// operation can succeed, and failing at construction beats failing later.
func NewStore(store *configstore.Store) *Store {
	if store == nil {
		panic("serverassets: nil configstore.Store")
	}
	return &Store{store: store}
}

// newUUID returns a random RFC 4122 version 4 UUID, using only the standard
// library so this package adds no direct dependency (google/uuid is only an
// indirect dependency of the module).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("serverassets: random source unavailable: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func nowUTC() time.Time { return time.Now().UTC() }

// snapshot is everything this package owns, as read in one Load, plus the
// exact stored payload bytes used as Expected guards on write.
type snapshot struct {
	servers   map[string]Server
	serverRaw map[string][]byte
	groups    map[string]ServerGroup
	groupRaw  map[string][]byte
	jumps     map[string]JumpHost
	jumpRaw   map[string][]byte
}

func (s *Store) load() (*snapshot, error) {
	entries, _, err := s.store.Load()
	if err != nil {
		return nil, err
	}
	snap := &snapshot{
		servers:   map[string]Server{},
		serverRaw: map[string][]byte{},
		groups:    map[string]ServerGroup{},
		groupRaw:  map[string][]byte{},
		jumps:     map[string]JumpHost{},
		jumpRaw:   map[string][]byte{},
	}
	for _, entry := range entries {
		switch entry.Kind {
		case KindServer:
			var srv Server
			if err := json.Unmarshal(entry.Payload, &srv); err != nil {
				return nil, fmt.Errorf("serverassets: corrupt server record %s: %w", entry.ID, err)
			}
			snap.servers[entry.ID] = srv
			snap.serverRaw[entry.ID] = entry.Payload
		case KindServerGrp:
			var grp ServerGroup
			if err := json.Unmarshal(entry.Payload, &grp); err != nil {
				return nil, fmt.Errorf("serverassets: corrupt server-group record %s: %w", entry.ID, err)
			}
			snap.groups[entry.ID] = grp
			snap.groupRaw[entry.ID] = entry.Payload
		case KindJumpHost:
			var jump JumpHost
			if err := json.Unmarshal(entry.Payload, &jump); err != nil {
				return nil, fmt.Errorf("serverassets: corrupt jump-host record %s: %w", entry.ID, err)
			}
			snap.jumps[entry.ID] = jump
			snap.jumpRaw[entry.ID] = entry.Payload
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
	_, err := s.store.Commit(change)
	return err
}

// --- address handling ---

// normalizeAddress returns the canonical form used for duplicate detection:
// an IP in net.ParseIP canonical form, otherwise a lower-cased hostname
// without a trailing dot.
func normalizeAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", fmt.Errorf("%w: address is required", ErrValidation)
	}
	if ip := net.ParseIP(address); ip != nil {
		return ip.String(), nil
	}
	host, err := validateHostname(address)
	if err != nil {
		return "", err
	}
	return host, nil
}

func validateHostname(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || len(host) > 253 {
		return "", fmt.Errorf("%w: address is not a valid IP or hostname", ErrValidation)
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 {
			return "", fmt.Errorf("%w: address is not a valid IP or hostname", ErrValidation)
		}
		for i, c := range label {
			ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
			if !ok || i == 0 && c == '-' || i == len(label)-1 && c == '-' {
				return "", fmt.Errorf("%w: address is not a valid IP or hostname", ErrValidation)
			}
		}
	}
	return host, nil
}

func duplicateKey(address string, port int) string {
	return normalizeAddressOrSelf(address) + ":" + fmt.Sprint(port)
}

func normalizeAddressOrSelf(address string) string {
	if norm, err := normalizeAddress(address); err == nil {
		return norm
	}
	return strings.ToLower(strings.TrimSpace(address))
}

// duplicateServer returns a live server with the same normalised
// address+port, if any, excluding excludeID.
func (snap *snapshot) duplicateServer(address string, port int, excludeID string) *Server {
	key := duplicateKey(address, port)
	for _, srv := range snap.servers {
		if srv.ID == excludeID || srv.Deleted {
			continue
		}
		if duplicateKey(srv.Address, srv.Port) == key {
			found := srv
			return &found
		}
	}
	return nil
}

func validatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%w: port must be between 1 and 65535", ErrValidation)
	}
	return nil
}

func validateAuthMethod(method string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "", AuthPassword:
		return AuthPassword, nil
	case AuthKey:
		return AuthKey, nil
	default:
		return "", fmt.Errorf("%w: auth_method must be %q or %q", ErrValidation, AuthPassword, AuthKey)
	}
}

func validateSudoMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", SudoNone:
		return SudoNone, nil
	case SudoNoPasswd:
		return SudoNoPasswd, nil
	default:
		return "", fmt.Errorf("%w: sudo_mode must be %q or %q", ErrValidation, SudoNone, SudoNoPasswd)
	}
}

func cleanTags(tags []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	return out
}

func cleanStrings(in []string) []string {
	out := []string{}
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// validateServer normalises a server in place and checks required fields.
// References (group, jump host) are checked against snap when snap != nil.
func validateServer(srv *Server, snap *snapshot) error {
	srv.Name = strings.TrimSpace(srv.Name)
	if srv.Name == "" {
		return fmt.Errorf("%w: name is required", ErrValidation)
	}
	srv.Hostname = strings.TrimSpace(srv.Hostname)
	norm, err := normalizeAddress(srv.Address)
	if err != nil {
		return err
	}
	srv.Address = norm
	if err := validatePort(srv.Port); err != nil {
		return err
	}
	srv.User = strings.TrimSpace(srv.User)
	if srv.User == "" {
		return fmt.Errorf("%w: user is required", ErrValidation)
	}
	method, err := validateAuthMethod(srv.AuthMethod)
	if err != nil {
		return err
	}
	srv.AuthMethod = method
	mode, err := validateSudoMode(srv.SudoMode)
	if err != nil {
		return err
	}
	srv.SudoMode = mode
	srv.Tags = cleanTags(srv.Tags)
	srv.SudoCommands = cleanStrings(srv.SudoCommands)
	srv.IdentityFile = strings.TrimSpace(srv.IdentityFile)
	srv.CredentialRef = strings.TrimSpace(srv.CredentialRef)
	srv.GroupID = strings.TrimSpace(srv.GroupID)
	srv.JumpHostID = strings.TrimSpace(srv.JumpHostID)
	srv.OS = strings.TrimSpace(srv.OS)
	srv.Notes = strings.TrimSpace(srv.Notes)
	if snap != nil {
		if srv.GroupID != "" {
			if _, ok := snap.groups[srv.GroupID]; !ok {
				return fmt.Errorf("%w: group does not exist", ErrValidation)
			}
		}
		if srv.JumpHostID != "" {
			if _, ok := snap.jumps[srv.JumpHostID]; !ok {
				return fmt.Errorf("%w: jump host does not exist", ErrValidation)
			}
		}
	}
	return nil
}

// --- servers ---

// CreateServer stores a new server, generating its permanent UUID.
// A live server with the same normalised address+port is ErrDuplicate.
// Soft-deleted servers do not count towards duplication.
func (s *Store) CreateServer(srv Server) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return Server{}, err
	}
	srv.ID = ""
	if err := validateServer(&srv, snap); err != nil {
		return Server{}, err
	}
	if dup := snap.duplicateServer(srv.Address, srv.Port, ""); dup != nil {
		return Server{}, fmt.Errorf("%w: a server already uses this address and port", ErrDuplicate)
	}
	now := nowUTC()
	srv.ID = newUUID()
	srv.CreatedAt = now
	srv.UpdatedAt = now
	srv.Deleted = false
	srv.DeletedAt = nil
	entry, err := putEntry(KindServer, srv.ID, srv)
	if err != nil {
		return Server{}, err
	}
	key := configstore.Key{Kind: KindServer, ID: srv.ID}
	if err := s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: nil},
	}); err != nil {
		return Server{}, err
	}
	return srv, nil
}

// UpdateServer replaces a server's fields. ID, CreatedAt, Deleted/DeletedAt
// and LastCheck are immutable here (use DeleteServer/RestoreServer/
// SetCheckResult for those). Changing IP/port is the caller's cue to re-test
// the connection and re-verify the host key; this method only stores fields.
func (s *Store) UpdateServer(srv Server) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(srv.ID) == "" {
		return Server{}, ErrNotFound
	}
	snap, err := s.load()
	if err != nil {
		return Server{}, err
	}
	old, ok := snap.servers[srv.ID]
	if !ok || old.Deleted {
		return Server{}, ErrNotFound
	}
	if err := validateServer(&srv, snap); err != nil {
		return Server{}, err
	}
	if dup := snap.duplicateServer(srv.Address, srv.Port, srv.ID); dup != nil {
		return Server{}, fmt.Errorf("%w: a server already uses this address and port", ErrDuplicate)
	}
	srv.ID = old.ID
	srv.CreatedAt = old.CreatedAt
	srv.Deleted = old.Deleted
	srv.DeletedAt = old.DeletedAt
	srv.LastCheck = old.LastCheck
	srv.UpdatedAt = nowUTC()
	entry, err := putEntry(KindServer, srv.ID, srv)
	if err != nil {
		return Server{}, err
	}
	key := configstore.Key{Kind: KindServer, ID: srv.ID}
	if err := s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: snap.serverRaw[srv.ID]},
	}); err != nil {
		return Server{}, err
	}
	return srv, nil
}

// GetServer returns one server by UUID, deleted ones included.
func (s *Store) GetServer(id string) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return Server{}, err
	}
	srv, ok := snap.servers[id]
	if !ok {
		return Server{}, ErrNotFound
	}
	return srv, nil
}

// ListServers returns servers matching f, sorted by name (case-insensitive,
// stable; ID breaks ties). Deleted servers are excluded unless requested.
func (s *Store) ListServers(f Filter) ([]Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return nil, err
	}
	status := strings.ToLower(strings.TrimSpace(f.Status))
	query := strings.ToLower(strings.TrimSpace(f.Query))
	out := []Server{}
	for _, srv := range snap.servers {
		if srv.Deleted && !f.IncludeDeleted {
			continue
		}
		if f.GroupID != "" && srv.GroupID != f.GroupID {
			continue
		}
		if f.Tag != "" {
			found := false
			for _, tag := range srv.Tags {
				if strings.EqualFold(tag, f.Tag) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if f.OS != "" && !strings.EqualFold(srv.OS, f.OS) {
			continue
		}
		switch status {
		case "", StatusUnknown, StatusOK, StatusFailed:
		default:
			return nil, fmt.Errorf("%w: status must be %q, %q or %q", ErrValidation, StatusUnknown, StatusOK, StatusFailed)
		}
		switch status {
		case StatusUnknown:
			if srv.LastCheck != nil {
				continue
			}
		case StatusOK:
			if srv.LastCheck == nil || !srv.LastCheck.OK {
				continue
			}
		case StatusFailed:
			if srv.LastCheck == nil || srv.LastCheck.OK {
				continue
			}
		}
		if query != "" {
			hay := strings.ToLower(srv.Name + "\n" + srv.Hostname + "\n" + srv.Address)
			if !strings.Contains(hay, query) {
				continue
			}
		}
		out = append(out, srv)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name); a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// DeleteServer soft-deletes a server: it is hidden from default lists and
// stops being scheduled, but its record, metrics and task history stay
// linked by UUID. Deleting an already-deleted server is a no-op.
func (s *Store) DeleteServer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return err
	}
	srv, ok := snap.servers[id]
	if !ok {
		return ErrNotFound
	}
	if srv.Deleted {
		return nil
	}
	now := nowUTC()
	srv.Deleted = true
	srv.DeletedAt = &now
	srv.UpdatedAt = now
	entry, err := putEntry(KindServer, srv.ID, srv)
	if err != nil {
		return err
	}
	key := configstore.Key{Kind: KindServer, ID: srv.ID}
	return s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: snap.serverRaw[srv.ID]},
	})
}

// RestoreServer reverses a soft delete. It fails with ErrDuplicate if a
// live server has since taken the same address+port.
func (s *Store) RestoreServer(id string) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return Server{}, err
	}
	srv, ok := snap.servers[id]
	if !ok {
		return Server{}, ErrNotFound
	}
	if !srv.Deleted {
		return srv, nil
	}
	if dup := snap.duplicateServer(srv.Address, srv.Port, srv.ID); dup != nil {
		return Server{}, fmt.Errorf("%w: a server already uses this address and port", ErrDuplicate)
	}
	srv.Deleted = false
	srv.DeletedAt = nil
	srv.UpdatedAt = nowUTC()
	entry, err := putEntry(KindServer, srv.ID, srv)
	if err != nil {
		return Server{}, err
	}
	key := configstore.Key{Kind: KindServer, ID: srv.ID}
	if err := s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: snap.serverRaw[srv.ID]},
	}); err != nil {
		return Server{}, err
	}
	return srv, nil
}

// SetCheckResult records the latest connectivity check for a server
// without touching any other field. Deleted servers cannot be checked.
func (s *Store) SetCheckResult(id string, result CheckResult) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return Server{}, err
	}
	srv, ok := snap.servers[id]
	if !ok || srv.Deleted {
		return Server{}, ErrNotFound
	}
	if result.At.IsZero() {
		result.At = nowUTC()
	}
	srv.LastCheck = &result
	srv.UpdatedAt = nowUTC()
	entry, err := putEntry(KindServer, srv.ID, srv)
	if err != nil {
		return Server{}, err
	}
	key := configstore.Key{Kind: KindServer, ID: srv.ID}
	if err := s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: snap.serverRaw[srv.ID]},
	}); err != nil {
		return Server{}, err
	}
	return srv, nil
}

// --- groups ---

func validateGroup(grp *ServerGroup) error {
	grp.Name = strings.TrimSpace(grp.Name)
	if grp.Name == "" {
		return fmt.Errorf("%w: group name is required", ErrValidation)
	}
	grp.Description = strings.TrimSpace(grp.Description)
	return nil
}

func (snap *snapshot) groupNameTaken(name, excludeID string) bool {
	for _, grp := range snap.groups {
		if grp.ID != excludeID && strings.EqualFold(grp.Name, name) {
			return true
		}
	}
	return false
}

// CreateGroup stores a new group with a generated UUID. Group names are
// unique case-insensitively, so import can resolve a group by name.
func (s *Store) CreateGroup(grp ServerGroup) (ServerGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return ServerGroup{}, err
	}
	if err := validateGroup(&grp); err != nil {
		return ServerGroup{}, err
	}
	if snap.groupNameTaken(grp.Name, "") {
		return ServerGroup{}, fmt.Errorf("%w: a group with this name already exists", ErrDuplicate)
	}
	now := nowUTC()
	grp.ID = newUUID()
	grp.CreatedAt = now
	grp.UpdatedAt = now
	entry, err := putEntry(KindServerGrp, grp.ID, grp)
	if err != nil {
		return ServerGroup{}, err
	}
	key := configstore.Key{Kind: KindServerGrp, ID: grp.ID}
	if err := s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: nil},
	}); err != nil {
		return ServerGroup{}, err
	}
	return grp, nil
}

// UpdateGroup renames or re-describes a group; ID and CreatedAt are immutable.
func (s *Store) UpdateGroup(grp ServerGroup) (ServerGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(grp.ID) == "" {
		return ServerGroup{}, ErrNotFound
	}
	snap, err := s.load()
	if err != nil {
		return ServerGroup{}, err
	}
	old, ok := snap.groups[grp.ID]
	if !ok {
		return ServerGroup{}, ErrNotFound
	}
	if err := validateGroup(&grp); err != nil {
		return ServerGroup{}, err
	}
	if snap.groupNameTaken(grp.Name, grp.ID) {
		return ServerGroup{}, fmt.Errorf("%w: a group with this name already exists", ErrDuplicate)
	}
	grp.ID = old.ID
	grp.CreatedAt = old.CreatedAt
	grp.UpdatedAt = nowUTC()
	entry, err := putEntry(KindServerGrp, grp.ID, grp)
	if err != nil {
		return ServerGroup{}, err
	}
	key := configstore.Key{Kind: KindServerGrp, ID: grp.ID}
	if err := s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: snap.groupRaw[grp.ID]},
	}); err != nil {
		return ServerGroup{}, err
	}
	return grp, nil
}

// GetGroup returns one group by ID.
func (s *Store) GetGroup(id string) (ServerGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return ServerGroup{}, err
	}
	grp, ok := snap.groups[id]
	if !ok {
		return ServerGroup{}, ErrNotFound
	}
	return grp, nil
}

// ListGroups returns all groups sorted by name (case-insensitive, stable).
func (s *Store) ListGroups() ([]ServerGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]ServerGroup, 0, len(snap.groups))
	for _, grp := range snap.groups {
		out = append(out, grp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name); a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// DeleteGroup removes a group and clears GroupID on its servers, in one
// commit, so no server is left pointing at a missing group. Servers are
// never deleted by this operation.
func (s *Store) DeleteGroup(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := snap.groups[id]; !ok {
		return ErrNotFound
	}
	change := configstore.Change{
		Delete:   []configstore.Key{{Kind: KindServerGrp, ID: id}},
		Expected: map[configstore.Key][]byte{{Kind: KindServerGrp, ID: id}: snap.groupRaw[id]},
	}
	for _, srv := range snap.servers {
		if srv.GroupID != id {
			continue
		}
		srv.GroupID = ""
		srv.UpdatedAt = nowUTC()
		entry, err := putEntry(KindServer, srv.ID, srv)
		if err != nil {
			return err
		}
		change.Put = append(change.Put, entry)
		change.Expected[configstore.Key{Kind: KindServer, ID: srv.ID}] = snap.serverRaw[srv.ID]
	}
	return s.commit(change)
}

// --- jump hosts ---

func validateJumpHost(jump *JumpHost) error {
	jump.Name = strings.TrimSpace(jump.Name)
	if jump.Name == "" {
		return fmt.Errorf("%w: jump host name is required", ErrValidation)
	}
	norm, err := normalizeAddress(jump.Address)
	if err != nil {
		return err
	}
	jump.Address = norm
	if err := validatePort(jump.Port); err != nil {
		return err
	}
	jump.User = strings.TrimSpace(jump.User)
	if jump.User == "" {
		return fmt.Errorf("%w: jump host user is required", ErrValidation)
	}
	method, err := validateAuthMethod(jump.AuthMethod)
	if err != nil {
		return err
	}
	jump.AuthMethod = method
	jump.IdentityFile = strings.TrimSpace(jump.IdentityFile)
	jump.CredentialRef = strings.TrimSpace(jump.CredentialRef)
	jump.Notes = strings.TrimSpace(jump.Notes)
	return nil
}

// CreateJumpHost stores a new jump host with a generated UUID.
func (s *Store) CreateJumpHost(jump JumpHost) (JumpHost, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.load()
	if err != nil {
		return JumpHost{}, err
	}
	if err := validateJumpHost(&jump); err != nil {
		return JumpHost{}, err
	}
	now := nowUTC()
	jump.ID = newUUID()
	jump.CreatedAt = now
	jump.UpdatedAt = now
	entry, err := putEntry(KindJumpHost, jump.ID, jump)
	if err != nil {
		return JumpHost{}, err
	}
	key := configstore.Key{Kind: KindJumpHost, ID: jump.ID}
	if err := s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: nil},
	}); err != nil {
		return JumpHost{}, err
	}
	return jump, nil
}

// UpdateJumpHost replaces a jump host's fields; ID and CreatedAt are immutable.
func (s *Store) UpdateJumpHost(jump JumpHost) (JumpHost, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(jump.ID) == "" {
		return JumpHost{}, ErrNotFound
	}
	snap, err := s.load()
	if err != nil {
		return JumpHost{}, err
	}
	old, ok := snap.jumps[jump.ID]
	if !ok {
		return JumpHost{}, ErrNotFound
	}
	if err := validateJumpHost(&jump); err != nil {
		return JumpHost{}, err
	}
	jump.ID = old.ID
	jump.CreatedAt = old.CreatedAt
	jump.UpdatedAt = nowUTC()
	entry, err := putEntry(KindJumpHost, jump.ID, jump)
	if err != nil {
		return JumpHost{}, err
	}
	key := configstore.Key{Kind: KindJumpHost, ID: jump.ID}
	if err := s.commit(configstore.Change{
		Put:      []configstore.Entry{entry},
		Expected: map[configstore.Key][]byte{key: snap.jumpRaw[jump.ID]},
	}); err != nil {
		return JumpHost{}, err
	}
	return jump, nil
}

// GetJumpHost returns one jump host by ID.
func (s *Store) GetJumpHost(id string) (JumpHost, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return JumpHost{}, err
	}
	jump, ok := snap.jumps[id]
	if !ok {
		return JumpHost{}, ErrNotFound
	}
	return jump, nil
}

// ListJumpHosts returns all jump hosts sorted by name (case-insensitive).
func (s *Store) ListJumpHosts() ([]JumpHost, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]JumpHost, 0, len(snap.jumps))
	for _, jump := range snap.jumps {
		out = append(out, jump)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name); a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// DeleteJumpHost removes a jump host. It fails with ErrJumpHostInUse if any
// server (live or soft-deleted, so a later restore cannot dangle) still
// references it.
func (s *Store) DeleteJumpHost(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := snap.jumps[id]; !ok {
		return ErrNotFound
	}
	for _, srv := range snap.servers {
		if srv.JumpHostID == id {
			return ErrJumpHostInUse
		}
	}
	return s.commit(configstore.Change{
		Delete:   []configstore.Key{{Kind: KindJumpHost, ID: id}},
		Expected: map[configstore.Key][]byte{{Kind: KindJumpHost, ID: id}: snap.jumpRaw[id]},
	})
}
