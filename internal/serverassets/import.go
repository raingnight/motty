package serverassets

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Import columns. Header matching is case-insensitive and ignores spaces,
// hyphens and underscores ("Auth Method" matches auth_method).
//
//	name, hostname, address, port, user, auth_method, identity_file,
//	group, tags (semicolon-separated), notes, jump_host (jump host Name),
//	os
//
// There is deliberately no password/key column: import never carries
// secrets. Any credential-like column rejects the whole batch (see
// ErrPasswordColumn). Passwords and key passphrases are set separately in
// the vault under PasswordCredentialName/KeyPassphraseName.
var (
	// ErrPasswordColumn means the file contains a credential column.
	ErrPasswordColumn = errors.New("serverassets: import file contains a credential column; passwords and key passphrases are never imported")
	// ErrMissingColumns means a required header is absent.
	ErrMissingColumns = errors.New("serverassets: import file is missing required columns")
)

// credentialColumns are header names (normalised) that reject the batch.
var credentialColumns = map[string]bool{
	"password": true, "passwd": true, "pwd": true,
	"credential": true, "credentials": true,
	"passphrase": true, "secret": true,
	"privatekey": true, "privatekeyfile": true,
}

// importColumns maps a normalised header to the ImportRow field it fills.
var importColumns = map[string]string{
	"name": "name", "hostname": "hostname", "address": "address",
	"port": "port", "user": "user", "authmethod": "auth_method",
	"identityfile": "identity_file", "group": "group", "tags": "tags",
	"notes": "notes", "jumphost": "jump_host", "os": "os",
}

func normalizeHeader(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.ReplaceAll(h, " ", "")
	h = strings.ReplaceAll(h, "-", "")
	h = strings.ReplaceAll(h, "_", "")
	return h
}

// ImportRow is one parsed import line. Line is the 1-based data line
// (header is line 1). GroupID/JumpHostID are filled by PreviewImport when
// the named group/jump host already exists; a group that does not exist yet
// is created by ApplyImport and listed in ImportPreview.NewGroups.
type ImportRow struct {
	Line         int
	Name         string
	Hostname     string
	Address      string
	Port         int
	User         string
	AuthMethod   string
	IdentityFile string
	GroupName    string
	GroupID      string
	Tags         []string
	Notes        string
	JumpHostName string
	JumpHostID   string
	OS           string
}

func parsePortCell(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 22 // empty port means the SSH default
	}
	if n, err := strconv.Atoi(text); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil && f == float64(int(f)) {
		return int(f)
	}
	return -1 // unparsable: Validate flags the row as failed
}

func rowFromCells(cells []string, columns map[string]int, line int) ImportRow {
	get := func(field string) string {
		idx, ok := columns[field]
		if !ok || idx >= len(cells) {
			return ""
		}
		return strings.TrimSpace(cells[idx])
	}
	row := ImportRow{
		Line:         line,
		Name:         get("name"),
		Hostname:     get("hostname"),
		Address:      get("address"),
		Port:         parsePortCell(get("port")),
		User:         get("user"),
		AuthMethod:   strings.ToLower(get("auth_method")),
		IdentityFile: get("identity_file"),
		GroupName:    get("group"),
		Notes:        get("notes"),
		JumpHostName: get("jump_host"),
		OS:           get("os"),
	}
	if row.AuthMethod == "" {
		row.AuthMethod = AuthPassword
	}
	for _, tag := range strings.Split(get("tags"), ";") {
		if tag = strings.TrimSpace(tag); tag != "" {
			row.Tags = append(row.Tags, tag)
		}
	}
	return row
}

// checkHeaders validates a header row and returns the column map
// (field -> cell index). It rejects credential columns outright.
func checkHeaders(header []string) (map[string]int, error) {
	columns := map[string]int{}
	for i, h := range header {
		norm := normalizeHeader(h)
		if norm == "" {
			continue
		}
		if credentialColumns[norm] {
			return nil, fmt.Errorf("%w: column %q", ErrPasswordColumn, strings.TrimSpace(h))
		}
		if field, ok := importColumns[norm]; ok {
			if _, dup := columns[field]; !dup {
				columns[field] = i
			}
		}
		// Unknown columns are ignored.
	}
	if _, ok := columns["name"]; !ok {
		return nil, fmt.Errorf("%w: name", ErrMissingColumns)
	}
	if _, ok := columns["address"]; !ok {
		return nil, fmt.Errorf("%w: address", ErrMissingColumns)
	}
	return columns, nil
}

func rowIsEmpty(cells []string) bool {
	for _, c := range cells {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// ParseCSV parses CSV import data (encoding/csv). The first row is the
// header. A credential column rejects the whole batch; a missing name or
// address column does the same. Per-row problems are left for PreviewImport.
func ParseCSV(r io.Reader) ([]ImportRow, error) {
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("serverassets: read CSV: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("%w: empty file", ErrMissingColumns)
	}
	columns, err := checkHeaders(records[0])
	if err != nil {
		return nil, err
	}
	var rows []ImportRow
	for i, record := range records[1:] {
		if rowIsEmpty(record) {
			continue
		}
		rows = append(rows, rowFromCells(record, columns, i+2))
	}
	return rows, nil
}

// --- preview / apply ---

// ImportSkipped is a row not imported because the asset already exists.
type ImportSkipped struct {
	Line   int
	Name   string
	Reason string
}

// ImportFailure is a row that failed validation.
type ImportFailure struct {
	Line   int
	Name   string
	Reason string
}

// ImportWarning is a non-fatal note on a row that is still imported.
type ImportWarning struct {
	Line   int
	Name   string
	Reason string
}

// ImportPreview is the result of validating parsed rows before anything is
// written. ToAdd rows are the only ones ApplyImport writes.
type ImportPreview struct {
	ToAdd     []ImportRow
	Skipped   []ImportSkipped
	Failed    []ImportFailure
	Warnings  []ImportWarning
	NewGroups []string // group names (first spelling) created by ApplyImport
}

// ImportResult is one row's outcome in an ImportSummary.
type ImportResult struct {
	Line   int
	Name   string
	Status string // "added", "skipped" or "failed"
	Reason string
}

// ImportSummary is what ApplyImport returns: counts plus a reason per row.
type ImportSummary struct {
	Added   int
	Skipped int
	Failed  int
	Results []ImportResult
}

// PreviewImport validates rows against the current store without writing
// anything: duplicate detection is by normalised IP+port (an IP is
// canonicalised with net.ParseIP, a hostname lower-cased), both against
// existing live servers and within the file. A row whose address+port
// already exists is skipped, never overwriting the existing server's
// credential, fingerprint or connection settings. The same hostname with a
// different address is only a warning. A jump_host name that matches no
// jump host fails the row; a group name that matches no group is created
// by ApplyImport.
func (s *Store) PreviewImport(rows []ImportRow) (*ImportPreview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.load()
	if err != nil {
		return nil, err
	}
	preview := &ImportPreview{}
	groupByName := map[string]ServerGroup{}
	for _, grp := range snap.groups {
		groupByName[strings.ToLower(grp.Name)] = grp
	}
	jumpByName := map[string]JumpHost{}
	for _, jump := range snap.jumps {
		jumpByName[strings.ToLower(jump.Name)] = jump
	}
	// Hostnames already known, with the addresses they use.
	knownHostnames := map[string]map[string]bool{}
	for _, srv := range snap.servers {
		if srv.Deleted || srv.Hostname == "" {
			continue
		}
		host := strings.ToLower(strings.TrimSpace(srv.Hostname))
		if knownHostnames[host] == nil {
			knownHostnames[host] = map[string]bool{}
		}
		knownHostnames[host][normalizeAddressOrSelf(srv.Address)] = true
	}
	seenKeys := map[string]int{}
	seenHostnames := map[string]map[string]bool{}
	newGroupNames := map[string]bool{}

	for _, row := range rows {
		fail := func(reason string) {
			preview.Failed = append(preview.Failed, ImportFailure{Line: row.Line, Name: row.Name, Reason: reason})
		}
		skip := func(reason string) {
			preview.Skipped = append(preview.Skipped, ImportSkipped{Line: row.Line, Name: row.Name, Reason: reason})
		}
		if strings.TrimSpace(row.Name) == "" {
			fail("name is required")
			continue
		}
		normAddr, err := normalizeAddress(row.Address)
		if err != nil {
			fail("address is not a valid IP or hostname")
			continue
		}
		if err := validatePort(row.Port); err != nil {
			fail("port must be between 1 and 65535")
			continue
		}
		if strings.TrimSpace(row.User) == "" {
			fail("user is required")
			continue
		}
		if _, err := validateAuthMethod(row.AuthMethod); err != nil {
			fail("auth_method must be \"password\" or \"key\"")
			continue
		}
		if row.JumpHostName != "" {
			jump, ok := jumpByName[strings.ToLower(strings.TrimSpace(row.JumpHostName))]
			if !ok {
				fail("jump host not found")
				continue
			}
			row.JumpHostID = jump.ID
		}
		if row.GroupName != "" {
			if grp, ok := groupByName[strings.ToLower(strings.TrimSpace(row.GroupName))]; ok {
				row.GroupID = grp.ID
			} else if key := strings.ToLower(strings.TrimSpace(row.GroupName)); !newGroupNames[key] {
				newGroupNames[key] = true
				preview.NewGroups = append(preview.NewGroups, strings.TrimSpace(row.GroupName))
			}
		}
		key := normAddr + ":" + strconv.Itoa(row.Port)
		if line, seen := seenKeys[key]; seen {
			skip(fmt.Sprintf("duplicate of line %d in this file (same address and port)", line))
			continue
		}
		if dup := snap.duplicateServer(normAddr, row.Port, ""); dup != nil {
			skip("a server with this address and port already exists")
			continue
		}
		// Same hostname, different address: warning only, still imported.
		if host := strings.ToLower(strings.TrimSpace(row.Hostname)); host != "" {
			different := false
			for addr := range knownHostnames[host] {
				if addr != normAddr {
					different = true
				}
			}
			for addr := range seenHostnames[host] {
				if addr != normAddr {
					different = true
				}
			}
			if different {
				preview.Warnings = append(preview.Warnings, ImportWarning{
					Line: row.Line, Name: row.Name,
					Reason: "same hostname with a different address already exists (possible duplicate)",
				})
			}
			if seenHostnames[host] == nil {
				seenHostnames[host] = map[string]bool{}
			}
			seenHostnames[host][normAddr] = true
		}
		seenKeys[key] = row.Line
		row.Address = normAddr
		preview.ToAdd = append(preview.ToAdd, row)
	}
	return preview, nil
}

// ApplyImport writes only the preview's ToAdd rows (plus any new groups
// they reference). Duplicates that appeared since the preview are skipped
// rather than overwriting; other per-row errors are counted as failed.
// The summary carries a reason for every row.
func (s *Store) ApplyImport(preview *ImportPreview) (ImportSummary, error) {
	if preview == nil {
		return ImportSummary{}, errors.New("serverassets: nil import preview")
	}
	summary := ImportSummary{}
	for _, skipped := range preview.Skipped {
		summary.Skipped++
		summary.Results = append(summary.Results, ImportResult{Line: skipped.Line, Name: skipped.Name, Status: "skipped", Reason: skipped.Reason})
	}
	for _, failed := range preview.Failed {
		summary.Failed++
		summary.Results = append(summary.Results, ImportResult{Line: failed.Line, Name: failed.Name, Status: "failed", Reason: failed.Reason})
	}
	// Create referenced groups that do not exist yet.
	groupIDs := map[string]string{}
	for _, name := range preview.NewGroups {
		grp, err := s.CreateGroup(ServerGroup{Name: name})
		if err != nil {
			if errors.Is(err, ErrDuplicate) {
				// Created concurrently since the preview; resolve by name.
				if groups, listErr := s.ListGroups(); listErr == nil {
					for _, g := range groups {
						if strings.EqualFold(g.Name, name) {
							groupIDs[strings.ToLower(name)] = g.ID
							break
						}
					}
				}
				continue
			}
			return summary, fmt.Errorf("serverassets: create group for import: %w", err)
		}
		groupIDs[strings.ToLower(name)] = grp.ID
	}
	for _, row := range preview.ToAdd {
		srv := Server{
			Name: row.Name, Hostname: row.Hostname, Address: row.Address,
			Port: row.Port, User: row.User, AuthMethod: row.AuthMethod,
			IdentityFile: row.IdentityFile, GroupID: row.GroupID,
			Tags: row.Tags, Notes: row.Notes, JumpHostID: row.JumpHostID,
			OS: row.OS,
		}
		if srv.GroupID == "" && row.GroupName != "" {
			srv.GroupID = groupIDs[strings.ToLower(strings.TrimSpace(row.GroupName))]
		}
		if _, err := s.CreateServer(srv); err != nil {
			if errors.Is(err, ErrDuplicate) {
				summary.Skipped++
				summary.Results = append(summary.Results, ImportResult{Line: row.Line, Name: row.Name, Status: "skipped", Reason: "a server with this address and port already exists"})
				continue
			}
			summary.Failed++
			summary.Results = append(summary.Results, ImportResult{Line: row.Line, Name: row.Name, Status: "failed", Reason: err.Error()})
			continue
		}
		summary.Added++
		summary.Results = append(summary.Results, ImportResult{Line: row.Line, Name: row.Name, Status: "added"})
	}
	return summary, nil
}

// ImportCSV runs the whole pipeline for CSV data: parse, preview, apply.
func (s *Store) ImportCSV(r io.Reader) (ImportSummary, error) {
	rows, err := ParseCSV(r)
	if err != nil {
		return ImportSummary{}, err
	}
	preview, err := s.PreviewImport(rows)
	if err != nil {
		return ImportSummary{}, err
	}
	return s.ApplyImport(preview)
}

// ImportXLSX runs the whole pipeline for .xlsx data.
func (s *Store) ImportXLSX(data []byte) (ImportSummary, error) {
	rows, err := ParseXLSX(data)
	if err != nil {
		return ImportSummary{}, err
	}
	preview, err := s.PreviewImport(rows)
	if err != nil {
		return ImportSummary{}, err
	}
	return s.ApplyImport(preview)
}
