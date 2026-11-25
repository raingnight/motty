package serverassets

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"motty/internal/configstore"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	raw, err := configstore.Open(filepath.Join(dir, "config.db"), 1)
	if err != nil {
		t.Fatalf("open configstore: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return NewStore(raw)
}

func testServer(name, address string, port int) Server {
	return Server{Name: name, Address: address, Port: port, User: "root", AuthMethod: AuthPassword}
}

func mustCreate(t *testing.T, s *Store, srv Server) Server {
	t.Helper()
	created, err := s.CreateServer(srv)
	if err != nil {
		t.Fatalf("CreateServer(%s): %v", srv.Name, err)
	}
	return created
}

func TestCreateGeneratesUUIDAndPreservesItOnUpdate(t *testing.T) {
	s := openTestStore(t)
	created := mustCreate(t, s, testServer("web-1", "10.0.0.1", 22))
	if created.ID == "" {
		t.Fatal("ID is empty")
	}
	if len(created.ID) != 36 || created.ID[8] != '-' || created.ID[14] != '4' {
		t.Fatalf("ID %q is not a v4 UUID", created.ID)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatal("timestamps not set")
	}
	created.Name = "web-1-renamed"
	created.Address = "10.0.0.2"
	updated, err := s.UpdateServer(created)
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if updated.ID != created.ID {
		t.Fatalf("ID changed on update: %q -> %q", created.ID, updated.ID)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatal("CreatedAt changed on update")
	}
	if updated.Address != "10.0.0.2" {
		t.Fatalf("address not updated: %q", updated.Address)
	}
	// A caller-supplied ID is ignored: the system generates it.
	other := testServer("web-2", "10.0.0.3", 22)
	other.ID = "caller-supplied-id"
	created2 := mustCreate(t, s, other)
	if created2.ID == "caller-supplied-id" {
		t.Fatal("caller-supplied ID was honoured")
	}
}

func TestCreateValidation(t *testing.T) {
	s := openTestStore(t)
	cases := []struct {
		name string
		srv  Server
	}{
		{"no name", testServer("", "10.0.0.1", 22)},
		{"no address", testServer("a", "", 22)},
		{"bad address", testServer("a", "not a host!!", 22)},
		{"port zero", testServer("a", "10.0.0.1", 0)},
		{"port too big", testServer("a", "10.0.0.1", 65536)},
		{"no user", Server{Name: "a", Address: "10.0.0.1", Port: 22}},
		{"bad auth", Server{Name: "a", Address: "10.0.0.1", Port: 22, User: "root", AuthMethod: "cert"}},
		{"bad sudo", Server{Name: "a", Address: "10.0.0.1", Port: 22, User: "root", SudoMode: "always"}},
	}
	for _, tc := range cases {
		if _, err := s.CreateServer(tc.srv); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: got err %v, want ErrValidation", tc.name, err)
		}
	}
	// Hostnames are accepted, and normalised to lower case.
	created := mustCreate(t, s, testServer("host", "Web-01.Example.COM", 22))
	if created.Address != "web-01.example.com" {
		t.Fatalf("hostname not normalised: %q", created.Address)
	}
}

func TestDuplicateDetectionAndIPNormalisation(t *testing.T) {
	s := openTestStore(t)
	mustCreate(t, s, testServer("a", "10.0.0.1", 22))
	// Same IP, different textual form (leading zeros are not valid in
	// net.ParseIP, so use the IPv6-expanded form instead).
	if _, err := s.CreateServer(testServer("b", "10.0.0.1", 22)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("same IP+port: got %v, want ErrDuplicate", err)
	}
	// Different port is fine.
	mustCreate(t, s, testServer("c", "10.0.0.1", 2222))
	// IPv6: compressed and expanded forms are the same address.
	mustCreate(t, s, testServer("v6a", "2001:db8::1", 22))
	if _, err := s.CreateServer(testServer("v6b", "2001:0db8:0000:0000:0000:0000:0000:0001", 22)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("IPv6 expanded form: got %v, want ErrDuplicate", err)
	}
	// Hostname case-insensitively.
	mustCreate(t, s, testServer("h1", "host.example.com", 22))
	if _, err := s.CreateServer(testServer("h2", "HOST.EXAMPLE.COM", 22)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("hostname case: got %v, want ErrDuplicate", err)
	}
	// A soft-deleted server does not block reuse, but restoring it then
	// conflicts with the live replacement.
	del := mustCreate(t, s, testServer("gone", "10.0.0.9", 22))
	if err := s.DeleteServer(del.ID); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	mustCreate(t, s, testServer("replacement", "10.0.0.9", 22))
	if _, err := s.RestoreServer(del.ID); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("restore over live duplicate: got %v, want ErrDuplicate", err)
	}
}

func TestSoftDeleteAndRestore(t *testing.T) {
	s := openTestStore(t)
	created := mustCreate(t, s, testServer("a", "10.0.0.1", 22))
	if err := s.DeleteServer(created.ID); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	got, err := s.GetServer(created.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if !got.Deleted || got.DeletedAt == nil {
		t.Fatalf("soft delete not recorded: %+v", got)
	}
	list, err := s.ListServers(Filter{})
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("deleted server still listed: %d", len(list))
	}
	list, err = s.ListServers(Filter{IncludeDeleted: true})
	if err != nil || len(list) != 1 {
		t.Fatalf("IncludeDeleted list = %d, err %v", len(list), err)
	}
	restored, err := s.RestoreServer(created.ID)
	if err != nil {
		t.Fatalf("RestoreServer: %v", err)
	}
	if restored.Deleted || restored.DeletedAt != nil {
		t.Fatal("restore did not clear deleted state")
	}
	list, _ = s.ListServers(Filter{})
	if len(list) != 1 {
		t.Fatalf("restored server not listed: %d", len(list))
	}
	if err := s.DeleteServer("missing-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: got %v, want ErrNotFound", err)
	}
	if _, err := s.RestoreServer("missing-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restore missing: got %v, want ErrNotFound", err)
	}
}

func TestFilterConditions(t *testing.T) {
	s := openTestStore(t)
	grp := mustCreateGroup(t, s, "prod")
	other := mustCreateGroup(t, s, "dev")
	a := mustCreate(t, s, Server{Name: "Alpha", Hostname: "alpha", Address: "10.0.0.1", Port: 22, User: "root", GroupID: grp.ID, Tags: []string{"web", "eu"}, OS: "Ubuntu 24.04"})
	b := mustCreate(t, s, Server{Name: "beta", Hostname: "beta", Address: "10.0.0.2", Port: 22, User: "root", GroupID: other.ID, Tags: []string{"db"}, OS: "Debian 12"})
	c := mustCreate(t, s, Server{Name: "Gamma", Hostname: "gamma", Address: "10.0.0.3", Port: 22, User: "root", OS: "Ubuntu 24.04"})
	if _, err := s.SetCheckResult(a.ID, CheckResult{At: time.Now(), OK: true, Stage: "dial"}); err != nil {
		t.Fatalf("SetCheckResult: %v", err)
	}
	if _, err := s.SetCheckResult(b.ID, CheckResult{At: time.Now(), OK: false, Stage: "dial", Error: "refused"}); err != nil {
		t.Fatalf("SetCheckResult: %v", err)
	}
	_ = c

	check := func(name string, f Filter, want ...string) {
		t.Helper()
		got, err := s.ListServers(f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: got %d servers %v, want %v", name, len(got), names(got), want)
		}
		for i, w := range want {
			if got[i].Name != w {
				t.Fatalf("%s: position %d got %q want %q", name, i, got[i].Name, w)
			}
		}
	}
	// Default sort is by name, case-insensitive.
	check("all", Filter{}, "Alpha", "beta", "Gamma")
	check("group", Filter{GroupID: grp.ID}, "Alpha")
	check("tag", Filter{Tag: "WEB"}, "Alpha")
	check("os", Filter{OS: "ubuntu 24.04"}, "Alpha", "Gamma")
	check("status ok", Filter{Status: "OK"}, "Alpha")
	check("status failed", Filter{Status: "Failed"}, "beta")
	check("status unknown", Filter{Status: "Unknown"}, "Gamma")
	check("query name", Filter{Query: "ALP"}, "Alpha")
	check("query hostname", Filter{Query: "beta"}, "beta")
	check("query ip", Filter{Query: "10.0.0.3"}, "Gamma")
	check("combined", Filter{OS: "Ubuntu 24.04", Status: "ok"}, "Alpha")
	if _, err := s.ListServers(Filter{Status: "bogus"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad status: got %v, want ErrValidation", err)
	}
}

func names(servers []Server) []string {
	out := make([]string, len(servers))
	for i, srv := range servers {
		out[i] = srv.Name
	}
	return out
}

func mustCreateGroup(t *testing.T, s *Store, name string) ServerGroup {
	t.Helper()
	grp, err := s.CreateGroup(ServerGroup{Name: name})
	if err != nil {
		t.Fatalf("CreateGroup(%s): %v", name, err)
	}
	return grp
}

func TestGroupDeleteClearsReferences(t *testing.T) {
	s := openTestStore(t)
	grp := mustCreateGroup(t, s, "prod")
	srv := mustCreate(t, s, Server{Name: "a", Address: "10.0.0.1", Port: 22, User: "root", GroupID: grp.ID})
	if _, err := s.CreateGroup(ServerGroup{Name: "PROD"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate group name: got %v", err)
	}
	if err := s.DeleteGroup(grp.ID); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	got, err := s.GetServer(srv.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.GroupID != "" {
		t.Fatalf("server still references deleted group: %q", got.GroupID)
	}
	if _, err := s.GetGroup(grp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted group still readable: %v", err)
	}
	if err := s.DeleteGroup(grp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing group: got %v, want ErrNotFound", err)
	}
	// Update keeps ID/CreatedAt and rejects a taken name.
	g1 := mustCreateGroup(t, s, "one")
	g2 := mustCreateGroup(t, s, "two")
	g2.Name = "ONE"
	if _, err := s.UpdateGroup(g2); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("rename to taken name: got %v", err)
	}
	g1.Description = "updated"
	updated, err := s.UpdateGroup(g1)
	if err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}
	if updated.ID != g1.ID || !updated.CreatedAt.Equal(g1.CreatedAt) || updated.Description != "updated" {
		t.Fatalf("UpdateGroup changed identity: %+v", updated)
	}
}

func TestJumpHostCRUDAndReferencedDelete(t *testing.T) {
	s := openTestStore(t)
	jump, err := s.CreateJumpHost(JumpHost{Name: "bastion", Address: "203.0.113.1", Port: 22, User: "jump"})
	if err != nil {
		t.Fatalf("CreateJumpHost: %v", err)
	}
	srv := mustCreate(t, s, Server{Name: "a", Address: "10.0.0.1", Port: 22, User: "root", JumpHostID: jump.ID})
	if err := s.DeleteJumpHost(jump.ID); !errors.Is(err, ErrJumpHostInUse) {
		t.Fatalf("delete referenced jump host: got %v, want ErrJumpHostInUse", err)
	}
	// Even a soft-deleted server still blocks deletion (restore must not dangle).
	if err := s.DeleteServer(srv.ID); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if err := s.DeleteJumpHost(jump.ID); !errors.Is(err, ErrJumpHostInUse) {
		t.Fatalf("delete jump host referenced by deleted server: got %v", err)
	}
	updated, err := s.UpdateJumpHost(JumpHost{ID: jump.ID, Name: "bastion-2", Address: "203.0.113.1", Port: 2222, User: "jump"})
	if err != nil {
		t.Fatalf("UpdateJumpHost: %v", err)
	}
	if updated.ID != jump.ID || updated.Port != 2222 {
		t.Fatalf("UpdateJumpHost: %+v", updated)
	}
	// Point the (deleted) server elsewhere is impossible via UpdateServer;
	// instead verify an unreferenced jump host deletes cleanly.
	free, err := s.CreateJumpHost(JumpHost{Name: "free", Address: "203.0.113.2", Port: 22, User: "jump"})
	if err != nil {
		t.Fatalf("CreateJumpHost free: %v", err)
	}
	if err := s.DeleteJumpHost(free.ID); err != nil {
		t.Fatalf("DeleteJumpHost free: %v", err)
	}
	if _, err := s.GetJumpHost(free.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted jump host still readable: %v", err)
	}
}

func TestCredentialNaming(t *testing.T) {
	id := "11111111-2222-4333-8444-555555555555"
	if got := PasswordCredentialName(id); got != "server-credential:"+id {
		t.Fatalf("password name: %q", got)
	}
	if got := KeyPassphraseName(id); got != "server-passphrase:"+id {
		t.Fatalf("passphrase name: %q", got)
	}
	if got := CredentialNameFor(Server{ID: id, AuthMethod: AuthKey}); got != "server-passphrase:"+id {
		t.Fatalf("key server credential name: %q", got)
	}
	if got := CredentialNameFor(Server{ID: id, AuthMethod: AuthPassword}); got != "server-credential:"+id {
		t.Fatalf("password server credential name: %q", got)
	}
	// CredentialRef is stored as a reference only; creating a server never
	// persists a secret value.
	s := openTestStore(t)
	created := mustCreate(t, s, Server{Name: "a", Address: "10.0.0.1", Port: 22, User: "root", AuthMethod: AuthPassword, CredentialRef: PasswordCredentialName("placeholder")})
	if created.CredentialRef != "server-credential:placeholder" {
		t.Fatalf("CredentialRef not stored verbatim: %q", created.CredentialRef)
	}
}
