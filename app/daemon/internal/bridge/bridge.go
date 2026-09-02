// Package bridge wires the daemon together: it implements every socket API
// method on top of the notion client, the .rm parser, the renderers, the
// pairing client, and local storage.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/notion"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/pair"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/render"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/rm"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/socket"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/store"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/xochitl"
)

// requestTimeout bounds every handler's outbound work.
const requestTimeout = 120 * time.Second

// Bridge holds the daemon's dependencies.
type Bridge struct {
	Store  *store.Store
	Docs   *xochitl.Store
	Broker *pair.Client
	QRPath string

	// NewNotion builds an API client for a token; tests point it at a fake
	// server.
	NewNotion func(token string) *notion.Client
}

// New returns a bridge with production defaults.
func New(st *store.Store, docs *xochitl.Store, broker *pair.Client, qrPath string) *Bridge {
	return &Bridge{
		Store:     st,
		Docs:      docs,
		Broker:    broker,
		QRPath:    qrPath,
		NewNotion: notion.New,
	}
}

// RegisterAll installs every method on the socket server.
func (b *Bridge) RegisterAll(srv *socket.Server) {
	srv.Register("status", b.handleStatus)
	srv.Register("pair.start", b.handlePairStart)
	srv.Register("pair.poll", b.handlePairPoll)
	srv.Register("targets.list", b.handleTargetsList)
	srv.Register("targets.refresh", b.handleTargetsRefresh)
	srv.Register("send", b.handleSend)
	srv.Register("logout", b.handleLogout)
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), requestTimeout)
}

// accountClient returns an authed client for one connected account. An empty
// accountID means "the only account", which keeps single-account callers
// (and the -call CLI) working without having to name one.
func (b *Bridge) accountClient(accountID string) (*notion.Client, store.Account, error) {
	accounts := b.Store.Accounts()
	if len(accounts) == 0 {
		return nil, store.Account{}, errors.New("no Notion account connected — add one from the Send to Notion screen")
	}
	if accountID == "" {
		if len(accounts) > 1 {
			return nil, store.Account{}, errors.New("more than one account is connected — pick a page under the workspace you want")
		}
		accountID = accounts[0].ID
	}
	token := b.Store.TokenFor(accountID)
	if token == "" {
		return nil, store.Account{}, errors.New("that Notion account is no longer connected — add it again")
	}
	for _, a := range accounts {
		if a.ID == accountID {
			return b.NewNotion(token), a, nil
		}
	}
	return nil, store.Account{}, errors.New("unknown account")
}

// dropTokenOn401 converts a revoked token into a clean re-pair prompt,
// forgetting only the account that was revoked.
func (b *Bridge) dropTokenOn401(accountID string, err error) error {
	if errors.Is(err, notion.ErrUnauthorized) {
		b.Store.RemoveAccount(accountID)
		return errors.New("that Notion connection was revoked — add the account again")
	}
	return err
}

func (b *Bridge) handleStatus(json.RawMessage) (any, error) {
	accounts := b.Store.Accounts()
	// Workspace stays populated for the single-account case so existing
	// callers (and the -call CLI) keep reading something useful.
	workspace := ""
	if len(accounts) == 1 {
		workspace = accounts[0].Workspace
	}
	out := make([]map[string]string, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, map[string]string{"id": a.ID, "workspace": a.Workspace})
	}
	return map[string]any{
		"authed":    len(accounts) > 0,
		"workspace": workspace,
		"accounts":  out,
	}, nil
}

func (b *Bridge) handlePairStart(json.RawMessage) (any, error) {
	c, cancel := ctx()
	defer cancel()
	s, err := b.Broker.Start(c, b.QRPath)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (b *Bridge) handlePairPoll(params json.RawMessage) (any, error) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.Unmarshal(params, &req); err != nil || req.DeviceCode == "" {
		return nil, errors.New("pair.poll needs device_code")
	}
	c, cancel := ctx()
	defer cancel()
	r, err := b.Broker.Poll(c, req.DeviceCode)
	if err != nil {
		return nil, err
	}
	resp := map[string]string{"state": string(r.State)}
	if r.State == pair.StateOK {
		workspace := r.Workspace
		if workspace == "" {
			// The broker may not know the workspace name; ask Notion.
			if ws, err := b.NewNotion(r.Token).BotInfo(c); err == nil {
				workspace = ws
			}
		}
		acc, err := b.Store.AddAccount(r.Token, workspace)
		if err != nil {
			return nil, fmt.Errorf("store token: %w", err)
		}
		// The QR encodes a pairing URL that is now spent. Nothing needs it
		// again, so do not leave it sitting in the config directory.
		os.Remove(b.QRPath)
		resp["workspace"] = workspace
		resp["account_id"] = acc.ID
	}
	return resp, nil
}

func (b *Bridge) handleTargetsList(params json.RawMessage) (any, error) {
	var req struct {
		Query string `json:"query"`
	}
	if len(params) > 0 {
		json.Unmarshal(params, &req)
	}
	accounts := b.Store.Accounts()
	if len(accounts) == 0 {
		return nil, errors.New("no Notion account connected — add one from the Send to Notion screen")
	}

	c, cancel := ctx()
	defer cancel()

	// One group per account so the picker can show pages under their
	// workspace. A single failing account must not hide the others, so its
	// error rides along in the group instead of failing the whole call.
	groups := make([]map[string]any, 0, len(accounts))
	var firstErr error
	for _, a := range accounts {
		group := map[string]any{
			"account_id": a.ID,
			"workspace":  a.Workspace,
			"pages":      []notion.Page{},
		}
		client, _, err := b.accountClient(a.ID)
		if err == nil {
			var pages []notion.Page
			// 0 means the client's default cap; the picker scrolls, so
			// there is no reason to truncate what the user shared.
			pages, err = client.Search(c, req.Query, 0)
			if err == nil && pages != nil {
				group["pages"] = arrangePages(c, client, pages)
			}
		}
		if err != nil {
			err = b.dropTokenOn401(a.ID, err)
			group["error"] = err.Error()
			if firstErr == nil {
				firstErr = err
			}
		}
		groups = append(groups, group)
	}
	// Every account failed: that is a real error, not an empty picker.
	if firstErr != nil && len(groups) == 1 {
		return nil, firstErr
	}

	return map[string]any{"accounts": groups, "recent": b.recentsFor(accounts)}, nil
}

// handleTargetsRefresh re-reads each connection's workspace name from Notion
// — it may have been renamed since pairing — and then returns a freshly
// fetched target list.
func (b *Bridge) handleTargetsRefresh(params json.RawMessage) (any, error) {
	c, cancel := ctx()
	defer cancel()
	for _, a := range b.Store.Accounts() {
		client, _, err := b.accountClient(a.ID)
		if err != nil {
			continue
		}
		name, err := client.BotInfo(c)
		if err != nil {
			// A revoked connection is reported per-group by targets.list
			// below; anything else is transient and not worth failing on.
			b.dropTokenOn401(a.ID, err)
			continue
		}
		if name != "" {
			b.Store.SetWorkspace(a.ID, name)
		}
	}
	return b.handleTargetsList(params)
}

// arrangePages puts the flat search results into Notion's own shape: each
// page follows its parent, indented by Depth. Siblings keep the order they
// appear in on their parent page, which is the only ordering Notion's API
// exposes; top-level pages have no such order — a database view's order and
// the sidebar's are both unavailable — so they are sorted by title, which at
// least stays put between refreshes.
func arrangePages(c context.Context, client *notion.Client, pages []notion.Page) []notion.Page {
	byID := make(map[string]notion.Page, len(pages))
	for _, p := range pages {
		byID[p.ID] = p
	}
	// A page whose parent was not shared with the connection has nothing to
	// nest under, so it stands as a root.
	children := map[string][]notion.Page{}
	var roots []notion.Page
	for _, p := range pages {
		if _, ok := byID[p.ParentID]; p.ParentID != "" && ok {
			children[p.ParentID] = append(children[p.ParentID], p)
		} else {
			roots = append(roots, p)
		}
	}

	byTitle := func(list []notion.Page) {
		sort.SliceStable(list, func(i, j int) bool {
			return strings.ToLower(list[i].Title) < strings.ToLower(list[j].Title)
		})
	}
	byTitle(roots)
	for parentID, kids := range children {
		byTitle(kids)
		// Prefer the order the child pages actually appear in on the parent.
		order, err := client.ChildPageOrder(c, parentID)
		if err != nil {
			continue
		}
		rank := make(map[string]int, len(order))
		for i, id := range order {
			rank[id] = i
		}
		sort.SliceStable(kids, func(i, j int) bool {
			ri, oki := rank[kids[i].ID]
			rj, okj := rank[kids[j].ID]
			if oki != okj {
				return oki // pages found on the parent come first
			}
			if !oki {
				return false // both unknown: keep the title order
			}
			return ri < rj
		})
		children[parentID] = kids
	}

	out := make([]notion.Page, 0, len(pages))
	var walk func(p notion.Page, depth int)
	walk = func(p notion.Page, depth int) {
		p.Depth = depth
		out = append(out, p)
		// Depth is capped so a cycle in the data cannot recurse forever.
		if depth >= 8 {
			return
		}
		for _, kid := range children[p.ID] {
			walk(kid, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return out
}

// recentsFor returns the recents the picker can actually use. Entries stored
// before multi-account support carry no account id: adopt them when exactly
// one account is connected, and drop them otherwise rather than offering a
// row whose target workspace is unknowable.
func (b *Bridge) recentsFor(accounts []store.Account) []store.Recent {
	recents := b.Store.Recents()
	out := make([]store.Recent, 0, len(recents))
	known := make(map[string]store.Account, len(accounts))
	for _, a := range accounts {
		known[a.ID] = a
	}
	// Adopting a legacy entry can collide with a newer one for the same page,
	// so drop repeats — the list is newest first, and the first win keeps it
	// in the right place.
	seen := make(map[string]bool, len(recents))
	for _, r := range recents {
		if r.AccountID == "" {
			if len(accounts) != 1 {
				continue
			}
			r.AccountID = accounts[0].ID
		}
		a, ok := known[r.AccountID]
		if !ok {
			// The account was removed or revoked since.
			continue
		}
		key := r.AccountID + "|" + r.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		r.Workspace = a.Workspace
		out = append(out, r)
	}
	return out
}

// sendRequest is the wire shape of the send method.
type sendRequest struct {
	DocUUID      string `json:"doc_uuid"`
	PageRange    string `json:"page_range"`
	Format       string `json:"format"` // "svg" | "pdf"
	TargetPageID string `json:"target_page_id"`
	TargetTitle  string `json:"target_title"` // optional, for the recents cache
	TargetIcon   string `json:"target_icon"`
	AccountID    string `json:"account_id"` // optional when only one is connected
	AttachAs     string `json:"attach_as"`  // "embed" (default) | "file"
}

// upload is one rendered file on its way to Notion.
type upload struct {
	filename    string
	contentType string
	data        []byte
}

func (b *Bridge) handleSend(params json.RawMessage) (any, error) {
	var req sendRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("bad send request: %w", err)
	}
	if req.DocUUID == "" || req.TargetPageID == "" {
		return nil, errors.New("send needs doc_uuid and target_page_id")
	}
	if req.Format != "svg" && req.Format != "pdf" {
		return nil, fmt.Errorf("unknown format %q (want svg or pdf)", req.Format)
	}
	attachAs := notion.AttachEmbed
	switch req.AttachAs {
	case "", "embed":
	case "file":
		attachAs = notion.AttachFileBlock
	default:
		return nil, fmt.Errorf("unknown attach_as %q (want embed or file)", req.AttachAs)
	}
	client, account, err := b.accountClient(req.AccountID)
	if err != nil {
		return nil, err
	}

	uploads, err := b.render(req)
	if err != nil {
		return nil, err
	}

	c, cancel := ctx()
	defer cancel()

	blockURL, err := b.deliver(c, client, req, uploads, attachAs)
	if err != nil {
		return nil, b.dropTokenOn401(account.ID, err)
	}

	b.Store.AddRecent(store.Recent{
		ID:        req.TargetPageID,
		Title:     req.TargetTitle,
		Icon:      req.TargetIcon,
		AccountID: account.ID,
		Workspace: account.Workspace,
	})
	return map[string]any{"ok": true, "block_url": blockURL}, nil
}

// render turns the requested pages into the files to upload. A PDF is one
// file for the whole selection; SVG is single-page by nature, so it is one
// file per selected page.
func (b *Bridge) render(req sendRequest) ([]upload, error) {
	doc, err := b.Docs.Document(req.DocUUID)
	if err != nil {
		return nil, err
	}
	indices, err := xochitl.ParsePageRange(req.PageRange, len(doc.PageIDs))
	if err != nil {
		return nil, err
	}

	scenes := make([]*rm.Scene, 0, len(indices))
	for _, i := range indices {
		path, err := b.Docs.PagePath(doc.UUID, doc.PageIDs[i])
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read page %d: %w", i+1, err)
		}
		scene, err := rm.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("parse page %d: %w", i+1, err)
		}
		scenes = append(scenes, scene)
	}

	baseName := safeFilename(doc.Name)
	if req.Format == "pdf" {
		var buf bytes.Buffer
		if err := render.PDF(scenes, &buf); err != nil {
			return nil, err
		}
		return []upload{{baseName + ".pdf", "application/pdf", buf.Bytes()}}, nil
	}

	uploads := make([]upload, 0, len(scenes))
	for n, scene := range scenes {
		var buf bytes.Buffer
		if err := render.SVG(scene, &buf); err != nil {
			return nil, err
		}
		name := baseName + ".svg"
		if len(scenes) > 1 {
			name = fmt.Sprintf("%s p%d.svg", baseName, indices[n]+1)
		}
		uploads = append(uploads, upload{name, "image/svg+xml", buf.Bytes()})
	}
	return uploads, nil
}

// deliver uploads each rendered file and appends it to the page, returning a
// URL to the first block it created.
func (b *Bridge) deliver(c context.Context, client *notion.Client, req sendRequest, uploads []upload, attachAs notion.AttachAs) (string, error) {
	blockURL := ""
	for _, u := range uploads {
		uploadID, err := client.UploadFile(c, u.filename, u.contentType, bytes.NewReader(u.data))
		if err != nil {
			return "", err
		}
		url, err := client.AttachFile(c, req.TargetPageID, uploadID, u.filename, u.contentType, attachAs)
		if err != nil {
			return "", err
		}
		if blockURL == "" {
			blockURL = url
		}
	}
	return blockURL, nil
}

func (b *Bridge) handleLogout(params json.RawMessage) (any, error) {
	var req struct {
		AccountID string `json:"account_id"`
	}
	if len(params) > 0 {
		json.Unmarshal(params, &req)
	}
	if req.AccountID == "" {
		if err := b.Store.RemoveAllAccounts(); err != nil {
			return nil, err
		}
	} else if err := b.Store.RemoveAccount(req.AccountID); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// safeFilename keeps notebook names upload-friendly.
func safeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "notebook"
	}
	var out strings.Builder
	for _, r := range name {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r':
			out.WriteRune('-')
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// DefaultQRPath places the QR image next to the daemon's other state.
func DefaultQRPath(configDir string) string {
	return filepath.Join(configDir, "pair-qr.png")
}
