package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAccountLifecycle(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "notion-bridge"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts()) != 0 {
		t.Fatal("fresh store must have no accounts")
	}

	work, err := s.AddAccount("secret-work", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	personal, err := s.AddAccount("secret-personal", "Home")
	if err != nil {
		t.Fatal(err)
	}
	if work.ID == personal.ID {
		t.Fatal("accounts must get distinct ids")
	}
	if got := s.Accounts(); len(got) != 2 || got[0].Workspace != "Acme" || got[1].Workspace != "Home" {
		t.Fatalf("accounts = %+v, want Acme then Home", got)
	}
	if s.TokenFor(work.ID) != "secret-work" || s.TokenFor(personal.ID) != "secret-personal" {
		t.Error("tokens must be addressable per account")
	}
	if s.TokenFor("nope") != "" {
		t.Error("unknown account must have no token")
	}
	// Accounts() is what the UI sees; it must never carry tokens.
	for _, a := range s.Accounts() {
		if a.Token != "" {
			t.Errorf("Accounts() leaked a token for %s", a.Workspace)
		}
	}

	info, err := os.Stat(s.accountsPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("accounts file mode = %o, want 600", info.Mode().Perm())
	}

	// Re-pairing a connected workspace refreshes it rather than duplicating.
	again, err := s.AddAccount("secret-work-2", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != work.ID || len(s.Accounts()) != 2 {
		t.Errorf("re-pair should refresh in place, got %+v / %d accounts", again, len(s.Accounts()))
	}
	if s.TokenFor(work.ID) != "secret-work-2" {
		t.Error("re-pair should replace the token")
	}

	if err := s.RemoveAccount(work.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.Accounts(); len(got) != 1 || got[0].ID != personal.ID {
		t.Fatalf("accounts after removal = %+v", got)
	}
	if err := s.RemoveAccount(work.ID); err != nil {
		t.Errorf("removing an unknown account should be a no-op: %v", err)
	}
	if err := s.RemoveAllAccounts(); err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts()) != 0 {
		t.Error("accounts should be gone")
	}
	if err := s.RemoveAllAccounts(); err != nil {
		t.Errorf("double clear should be a no-op: %v", err)
	}
}

// A device paired before multi-account support has a `token`/`workspace`
// pair on disk; it must come back as one account rather than an empty list.
func TestMigratesLegacySingleToken(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("legacy-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workspace"), []byte("Old Workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	accs := s.Accounts()
	if len(accs) != 1 || accs[0].Workspace != "Old Workspace" {
		t.Fatalf("accounts = %+v, want the migrated workspace", accs)
	}
	if s.TokenFor(accs[0].ID) != "legacy-token" {
		t.Errorf("migrated token = %q", s.TokenFor(accs[0].ID))
	}
	// The legacy files are consumed, so migration runs exactly once.
	if _, err := os.Stat(filepath.Join(dir, "token")); !os.IsNotExist(err) {
		t.Error("legacy token file should be removed after migration")
	}
	if got := s.Accounts(); len(got) != 1 || got[0].ID != accs[0].ID {
		t.Errorf("account id must be stable across reads, got %+v", got)
	}
}

// recents.json is retired, but a device upgrading from a version that wrote
// one should not keep page titles in a file nothing reads.
func TestRemoveAllAccountsSweepsRetiredRecents(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "recents.json")
	if err := os.WriteFile(legacy, []byte(`[{"id":"p1","title":"Work page"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAllAccounts(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("retired recents.json should be removed, stat err = %v", err)
	}
}
