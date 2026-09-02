package store

import (
	"fmt"
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

func TestRemoveAccountDropsItsRecents(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.AddRecent(Recent{ID: "p1", Title: "Work page", AccountID: "acc-work"})
	s.AddRecent(Recent{ID: "p2", Title: "Home page", AccountID: "acc-home"})
	if err := s.RemoveAccount("acc-work"); err != nil {
		t.Fatal(err)
	}
	got := s.Recents()
	if len(got) != 1 || got[0].ID != "p2" {
		t.Fatalf("recents after removal = %+v, want only the other account's", got)
	}
}

// The same page id in two workspaces is two different targets.
func TestRecentsDedupIsPerAccount(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.AddRecent(Recent{ID: "shared", AccountID: "a"})
	s.AddRecent(Recent{ID: "shared", AccountID: "b"})
	if got := s.Recents(); len(got) != 2 {
		t.Fatalf("recents = %+v, want both accounts' entries", got)
	}
	s.AddRecent(Recent{ID: "shared", AccountID: "a"})
	if got := s.Recents(); len(got) != 2 {
		t.Fatalf("re-adding the same target should dedup, got %+v", got)
	}
}

func TestRecentsCapAndDedup(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		if err := s.AddRecent(Recent{ID: fmt.Sprintf("p%d", i), Title: fmt.Sprintf("Page %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	r := s.Recents()
	if len(r) != 10 {
		t.Fatalf("recents = %d, want cap of 10", len(r))
	}
	if r[0].ID != "p14" {
		t.Errorf("most recent first, got %s", r[0].ID)
	}

	// Re-adding an existing target moves it to the head without duplicating.
	if err := s.AddRecent(Recent{ID: "p10", Title: "Page 10"}); err != nil {
		t.Fatal(err)
	}
	r = s.Recents()
	if r[0].ID != "p10" || len(r) != 10 {
		t.Errorf("dedup failed: head=%s len=%d", r[0].ID, len(r))
	}
	count := 0
	for _, x := range r {
		if x.ID == "p10" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("p10 appears %d times", count)
	}
}
