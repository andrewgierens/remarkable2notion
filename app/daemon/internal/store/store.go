// Package store persists the daemon's small local state: the connected
// Notion accounts and their access tokens (mode 0600), and the
// recent-targets cache.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxRecents caps the recent-targets cache.
const maxRecents = 10

// Recent is one recently used target page. AccountID says which connected
// account the page belongs to — the same page id can exist in more than one
// workspace, and sending needs the right token.
type Recent struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Icon      string `json:"icon"`
	AccountID string `json:"account_id"`
	Workspace string `json:"workspace"`
}

// Account is one connected Notion workspace.
type Account struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Token     string `json:"-"` // never leaves the daemon
}

// accountRecord is the on-disk shape, token included.
type accountRecord struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Token     string `json:"token"`
}

// Store reads and writes state under one config directory.
type Store struct {
	mu  sync.Mutex
	dir string
}

// New returns a store rooted at dir, creating it if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) tokenPath() string     { return filepath.Join(s.dir, "token") }
func (s *Store) recentsPath() string   { return filepath.Join(s.dir, "recents.json") }
func (s *Store) workspacePath() string { return filepath.Join(s.dir, "workspace") }
func (s *Store) accountsPath() string  { return filepath.Join(s.dir, "accounts.json") }

// loadAccounts reads accounts.json, migrating a pre-multi-account single
// token file on first use so an already-paired device stays paired.
// Callers must hold s.mu.
func (s *Store) loadAccounts() []accountRecord {
	b, err := os.ReadFile(s.accountsPath())
	if err == nil {
		var accs []accountRecord
		if json.Unmarshal(b, &accs) == nil {
			return accs
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	tok, terr := os.ReadFile(s.tokenPath())
	if terr != nil {
		return nil
	}
	token := strings.TrimSpace(string(tok))
	if token == "" {
		return nil
	}
	ws, _ := os.ReadFile(s.workspacePath())
	migrated := []accountRecord{{
		ID:        newAccountID(),
		Workspace: strings.TrimSpace(string(ws)),
		Token:     token,
	}}
	if s.saveAccounts(migrated) == nil {
		os.Remove(s.tokenPath())
		os.Remove(s.workspacePath())
	}
	return migrated
}

// saveAccounts writes accounts.json. Callers must hold s.mu.
func (s *Store) saveAccounts(accs []accountRecord) error {
	if len(accs) == 0 {
		if err := os.Remove(s.accountsPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(accs)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.accountsPath(), b, 0o600)
}

// newAccountID returns a short random identifier for a connection. It only
// has to be unique within this device's account list.
func newAccountID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf[:])
}

// Accounts returns the connected accounts without their tokens, in the order
// they were added.
func (s *Store) Accounts() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.loadAccounts()
	out := make([]Account, 0, len(recs))
	for _, r := range recs {
		out = append(out, Account{ID: r.ID, Workspace: r.Workspace})
	}
	return out
}

// TokenFor returns the access token for one account, or "" when unknown.
func (s *Store) TokenFor(accountID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.loadAccounts() {
		if r.ID == accountID {
			return r.Token
		}
	}
	return ""
}

// AddAccount stores a newly paired account and returns it. Re-pairing a
// workspace that is already connected refreshes its token in place instead
// of listing it twice.
func (s *Store) AddAccount(token, workspace string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accs := s.loadAccounts()
	for i, r := range accs {
		if r.Workspace != "" && r.Workspace == workspace {
			accs[i].Token = token
			if err := s.saveAccounts(accs); err != nil {
				return Account{}, err
			}
			return Account{ID: accs[i].ID, Workspace: workspace}, nil
		}
	}
	rec := accountRecord{ID: newAccountID(), Workspace: workspace, Token: token}
	accs = append(accs, rec)
	if err := s.saveAccounts(accs); err != nil {
		return Account{}, err
	}
	return Account{ID: rec.ID, Workspace: rec.Workspace}, nil
}

// SetWorkspace updates a connection's workspace name, for when it was
// renamed in Notion since pairing.
func (s *Store) SetWorkspace(accountID, workspace string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accs := s.loadAccounts()
	for i, r := range accs {
		if r.ID == accountID {
			if r.Workspace == workspace {
				return nil
			}
			accs[i].Workspace = workspace
			return s.saveAccounts(accs)
		}
	}
	return nil
}

// RemoveAccount forgets one connection, along with its recents.
func (s *Store) RemoveAccount(accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accs := s.loadAccounts()
	kept := accs[:0:0]
	for _, r := range accs {
		if r.ID != accountID {
			kept = append(kept, r)
		}
	}
	if err := s.saveAccounts(kept); err != nil {
		return err
	}
	return s.pruneRecentsLocked(func(r Recent) bool { return r.AccountID != accountID })
}

// RemoveAllAccounts forgets every connection and the recents cache.
func (s *Store) RemoveAllAccounts() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range []string{s.accountsPath(), s.tokenPath(), s.workspacePath(), s.recentsPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Recents returns the cached recent targets, most recent first.
func (s *Store) Recents() []Recent {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.recentsPath())
	if err != nil {
		return nil
	}
	var r []Recent
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return r
}

// AddRecent puts a target at the head of the recents list, deduplicated and
// capped at maxRecents.
func (s *Store) AddRecent(r Recent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var current []Recent
	if b, err := os.ReadFile(s.recentsPath()); err == nil {
		json.Unmarshal(b, &current)
	}
	out := []Recent{r}
	for _, c := range current {
		// The same page id can exist in two connected workspaces, so a
		// recent is identified by account and page together.
		if (c.ID != r.ID || c.AccountID != r.AccountID) && len(out) < maxRecents {
			out = append(out, c)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.recentsPath(), b, 0o600)
}

// pruneRecentsLocked keeps only the recents satisfying keep. Callers must
// hold s.mu.
func (s *Store) pruneRecentsLocked(keep func(Recent) bool) error {
	b, err := os.ReadFile(s.recentsPath())
	if err != nil {
		return nil
	}
	var current []Recent
	if json.Unmarshal(b, &current) != nil {
		return nil
	}
	out := current[:0:0]
	for _, r := range current {
		if keep(r) {
			out = append(out, r)
		}
	}
	if len(out) == len(current) {
		return nil
	}
	if len(out) == 0 {
		if err := os.Remove(s.recentsPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	nb, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.recentsPath(), nb, 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
