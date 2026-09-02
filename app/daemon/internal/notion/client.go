// Package notion is a minimal Notion API client covering exactly what the
// bridge needs: page search, file uploads, and appending a file block.
package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strings"
	"time"
)

// idPattern matches a Notion object id: 32 hex digits, optionally dashed.
// Page ids arrive over the local socket and are interpolated into request
// paths, so they are checked rather than escaped — anything that is not an
// id is a bug or an attempt to reshape the request, and neither should reach
// Notion with the user's token attached.
var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)

// ErrBadID is returned for a page or block id that is not a Notion id.
var ErrBadID = errors.New("notion: not a valid page id")

func checkID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrBadID, id)
	}
	return nil
}

// APIVersion is sent as the Notion-Version header on every request.
const APIVersion = "2026-03-11"

// DefaultBaseURL is the production Notion API endpoint.
const DefaultBaseURL = "https://api.notion.com"

// ErrUnauthorized is returned on a 401 — the user revoked the connection in
// Notion settings. Callers should drop the stored token and prompt to re-pair.
var ErrUnauthorized = errors.New("notion: unauthorized (token revoked?)")

// Client talks to the Notion API on behalf of one paired workspace.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
}

// New returns a client for the production API.
func New(token string) *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 60 * time.Second},
		BaseURL: DefaultBaseURL,
		Token:   token,
	}
}

// Page is one target the user can send to.
type Page struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Icon  string `json:"icon"`
	// ParentID is the page this one sits under, when its parent is another
	// page. Empty for pages parented to a database, a block or the
	// workspace — Notion exposes no ordering or grouping for those.
	ParentID string `json:"-"`
	// Depth is how far the page is nested below a top-level one, filled in
	// once the list has been arranged into Notion's structure.
	Depth int `json:"depth"`
}

func (c *Client) do(ctx context.Context, method, path string, contentType string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Notion-Version", APIVersion)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiError(resp.StatusCode, data)
	}
	return data, nil
}

func apiError(status int, body []byte) error {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return fmt.Errorf("notion: %s (%d %s)", e.Message, status, e.Code)
	}
	return fmt.Errorf("notion: HTTP %d", status)
}

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, respBody any) error {
	var r io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	}
	data, err := c.do(ctx, method, path, "application/json", r)
	if err != nil {
		return err
	}
	if respBody != nil {
		if err := json.Unmarshal(data, respBody); err != nil {
			return fmt.Errorf("notion: decode response: %w", err)
		}
	}
	return nil
}

// searchPageSize is the largest page Notion's search endpoint will return.
const searchPageSize = 100

// DefaultSearchLimit caps how many pages Search collects when the caller does
// not ask for a specific number. Notion has no "everything in the workspace"
// grant — a connection only ever sees the pages shared with it — so this
// exists to bound the picker, not to hide pages the user chose to share.
const DefaultSearchLimit = 300

// searchResult is one entry of a /v1/search response.
type searchResult struct {
	Object string `json:"object"`
	ID     string `json:"id"`
	Icon   *struct {
		Type  string `json:"type"`
		Emoji string `json:"emoji"`
	} `json:"icon"`
	Properties map[string]struct {
		Type  string `json:"type"`
		Title []struct {
			PlainText string `json:"plain_text"`
		} `json:"title"`
	} `json:"properties"`
	Parent struct {
		Type   string `json:"type"`
		PageID string `json:"page_id"`
	} `json:"parent"`
}

// Search returns pages the integration can see, filtered by an optional
// query, most recently edited first. It follows Notion's cursor pagination
// until it has limit pages or the results run out, so a workspace that shared
// more pages than fit in one response is listed in full.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Page, error) {
	if limit <= 0 {
		limit = DefaultSearchLimit
	}

	pages := make([]Page, 0, min(limit, searchPageSize))
	cursor := ""
	for len(pages) < limit {
		req := map[string]any{
			"filter":    map[string]string{"property": "object", "value": "page"},
			"sort":      map[string]string{"direction": "descending", "timestamp": "last_edited_time"},
			"page_size": min(limit-len(pages), searchPageSize),
		}
		if query != "" {
			req["query"] = query
		}
		if cursor != "" {
			req["start_cursor"] = cursor
		}

		var resp struct {
			Results    []searchResult `json:"results"`
			HasMore    bool           `json:"has_more"`
			NextCursor string         `json:"next_cursor"`
		}
		if err := c.doJSON(ctx, http.MethodPost, "/v1/search", req, &resp); err != nil {
			return nil, err
		}
		pages = append(pages, collectPages(resp.Results)...)
		if !resp.HasMore || resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	return pages, nil
}

// collectPages turns search results into pages, dropping anything that is not
// a page and giving untitled pages a readable label.
func collectPages(results []searchResult) []Page {
	pages := make([]Page, 0, len(results))
	for _, r := range results {
		if r.Object != "page" {
			continue
		}
		p := Page{ID: r.ID}
		if r.Parent.Type == "page_id" {
			p.ParentID = r.Parent.PageID
		}
		for _, prop := range r.Properties {
			if prop.Type != "title" {
				continue
			}
			var b strings.Builder
			for _, t := range prop.Title {
				b.WriteString(t.PlainText)
			}
			p.Title = b.String()
			break
		}
		if p.Title == "" {
			p.Title = "Untitled"
		}
		if r.Icon != nil && r.Icon.Type == "emoji" {
			p.Icon = r.Icon.Emoji
		}
		pages = append(pages, p)
	}
	return pages
}

// ChildPageOrder returns the ids of the child pages and databases directly
// under a page, in the order they appear on it. This is the only place
// Notion exposes its own ordering: search can only sort by edit time, and
// neither the sidebar's order nor a database view's order is available.
func (c *Client) ChildPageOrder(ctx context.Context, pageID string) ([]string, error) {
	if err := checkID(pageID); err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"results"`
	}
	// One page of children is plenty to order the child pages of a page;
	// anything beyond it falls back to the caller's own ordering.
	err := c.doJSON(ctx, http.MethodGet, "/v1/blocks/"+pageID+"/children?page_size=100", nil, &resp)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.Type == "child_page" || r.Type == "child_database" {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

// BotInfo returns the workspace name this token is connected to.
func (c *Client) BotInfo(ctx context.Context) (workspace string, err error) {
	var resp struct {
		Bot struct {
			WorkspaceName string `json:"workspace_name"`
		} `json:"bot"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/users/me", nil, &resp); err != nil {
		return "", err
	}
	return resp.Bot.WorkspaceName, nil
}

// UploadFile pushes one file through the file_uploads API and returns the
// file_upload id, ready to be attached to a block. Single-part mode caps at
// 20 MB, which page renders will not approach.
func (c *Client) UploadFile(ctx context.Context, filename, contentType string, content io.Reader) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/v1/file_uploads", map[string]string{
		"mode":         "single_part",
		"filename":     filename,
		"content_type": contentType,
	}, &created)
	if err != nil {
		return "", fmt.Errorf("create upload: %w", err)
	}
	if created.ID == "" || strings.ContainsAny(created.ID, "/?#") {
		return "", fmt.Errorf("create upload: unusable upload id %q", created.ID)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// The form field must be named exactly "file". CreateFormFile would
	// hardcode application/octet-stream, which Notion rejects: the part's
	// Content-Type has to match the content_type declared when the upload
	// was created, so build the part header by hand.
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	hdr.Set("Content-Type", contentType)
	part, err := mw.CreatePart(hdr)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, content); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	var sent struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	data, err := c.do(ctx, http.MethodPost, "/v1/file_uploads/"+created.ID+"/send", mw.FormDataContentType(), &buf)
	if err != nil {
		return "", fmt.Errorf("send upload: %w", err)
	}
	if err := json.Unmarshal(data, &sent); err != nil {
		return "", fmt.Errorf("send upload: decode: %w", err)
	}
	if sent.Status != "" && sent.Status != "uploaded" {
		return "", fmt.Errorf("send upload: unexpected status %q", sent.Status)
	}
	return created.ID, nil
}

// AttachAs says how an upload should appear on the page.
type AttachAs string

const (
	// AttachEmbed renders the file on the page: a pdf block for PDFs, an
	// image block for images.
	AttachEmbed AttachAs = "embed"
	// AttachFileBlock adds a plain file block — a download row.
	AttachFileBlock AttachAs = "file"
)

// BlockTypeFor picks the block type for an upload. A "file" block is only
// ever a download row, so embedding puts PDFs in a "pdf" block and images
// (including SVG) in an "image" block. Notion enforces the pairing — a
// mismatched content type is rejected when the block is created.
func BlockTypeFor(contentType string, as AttachAs) string {
	if as == AttachFileBlock {
		return "file"
	}
	base, _, _ := strings.Cut(contentType, ";")
	switch base = strings.TrimSpace(base); {
	case base == "application/pdf":
		return "pdf"
	case strings.HasPrefix(base, "image/"):
		return "image"
	default:
		return "file"
	}
}

// fileBlockValue is the block body pointing at an uploaded file.
func fileBlockValue(blockType, fileUploadID, filename string) map[string]any {
	value := map[string]any{
		"type":        "file_upload",
		"file_upload": map[string]string{"id": fileUploadID},
	}
	// Only a plain file block shows a name, and it is the one case where the
	// filename is not otherwise visible on the page.
	if blockType == "file" && filename != "" {
		value["name"] = filename
	}
	return value
}

// BlockURL is the deep link to one block on a page.
func BlockURL(pageID, blockID string) string {
	compact := func(id string) string { return strings.ReplaceAll(id, "-", "") }
	url := "https://www.notion.so/" + compact(pageID)
	if blockID != "" {
		url += "#" + compact(blockID)
	}
	return url
}

// AttachFile appends a block referencing an uploaded file to a page and
// returns a URL to the new block. contentType and as together decide the
// block type, and so whether the file renders on the page or appears as a
// download row.
func (c *Client) AttachFile(ctx context.Context, pageID, fileUploadID, filename, contentType string, as AttachAs) (blockURL string, err error) {
	if err := checkID(pageID); err != nil {
		return "", err
	}
	blockType := BlockTypeFor(contentType, as)
	req := map[string]any{
		"children": []any{
			map[string]any{
				"object":  "block",
				"type":    blockType,
				blockType: fileBlockValue(blockType, fileUploadID, filename),
			},
		},
	}
	var resp struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := c.doJSON(ctx, http.MethodPatch, "/v1/blocks/"+pageID+"/children", req, &resp); err != nil {
		return "", err
	}
	blockID := ""
	if len(resp.Results) > 0 {
		blockID = resp.Results[0].ID
	}
	return BlockURL(pageID, blockID), nil
}
