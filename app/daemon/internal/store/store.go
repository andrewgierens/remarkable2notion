// Package store persists the daemon's small local state: the connected
// Notion accounts and their access tokens (mode 0600).
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

func (s *Store) tokenPath() string { return filepath.Join(s.dir, "token") }

// recentsPath is the retired recent-targets cache. Nothing writes it any
// more; it is still named here so an upgraded device stops carrying page
// titles around in a file nothing reads.
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

// RemoveAccount forgets one connection.
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
	return s.saveAccounts(kept)
}

// RemoveAllAccounts forgets every connection, and sweeps up the retired
// recents cache if this device still has one.
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

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
