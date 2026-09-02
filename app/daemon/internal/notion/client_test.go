package notion

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New("tok-123")
	c.BaseURL = srv.URL
	return c
}

func TestSearch(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/search" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-123" {
			t.Errorf("auth header = %q", got)
		}
		if got := r.Header.Get("Notion-Version"); got != APIVersion {
			t.Errorf("version header = %q", got)
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["query"] != "meeting" {
			t.Errorf("query = %v", req["query"])
		}
		io.WriteString(w, `{"results":[
			{"object":"page","id":"p1","icon":{"type":"emoji","emoji":"📓"},
			 "properties":{"title":{"type":"title","title":[{"plain_text":"Meeting "},{"plain_text":"notes"}]}}},
			{"object":"database","id":"d1"},
			{"object":"page","id":"p2","properties":{"Name":{"type":"title","title":[]}}}
		]}`)
	}))

	pages, err := c.Search(context.Background(), "meeting", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("got %d pages, want 2 (databases filtered out)", len(pages))
	}
	if pages[0].Title != "Meeting notes" || pages[0].Icon != "📓" {
		t.Errorf("page[0] = %+v", pages[0])
	}
	if pages[1].Title != "Untitled" {
		t.Errorf("empty title should fall back to Untitled, got %q", pages[1].Title)
	}
}

func TestUploadAndAttach(t *testing.T) {
	var sawCreate, sawSend, sawAttach bool
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/file_uploads" && r.Method == http.MethodPost:
			sawCreate = true
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			if req["mode"] != "single_part" || req["filename"] != "page.svg" {
				t.Errorf("create req = %v", req)
			}
			io.WriteString(w, `{"id":"fu-1","status":"pending"}`)
		case r.URL.Path == "/v1/file_uploads/fu-1/send" && r.Method == http.MethodPost:
			sawSend = true
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatalf("not multipart: %v", err)
			}
			f, hdr, err := r.FormFile("file")
			if err != nil {
				t.Fatalf("form field must be named exactly 'file': %v", err)
			}
			defer f.Close()
			body, _ := io.ReadAll(f)
			if string(body) != "<svg/>" || hdr.Filename != "page.svg" {
				t.Errorf("file part = %q (%s)", body, hdr.Filename)
			}
			// Notion rejects the send unless the part's Content-Type matches
			// the content_type declared when the upload was created.
			if got := hdr.Header.Get("Content-Type"); got != "image/svg+xml" {
				t.Errorf("file part Content-Type = %q, want image/svg+xml", got)
			}
			io.WriteString(w, `{"id":"fu-1","status":"uploaded"}`)
		case r.URL.Path == "/v1/blocks/11111111-2222-3333-4444-555555555555/children" && r.Method == http.MethodPatch:
			sawAttach = true
			var req struct {
				Children []map[string]any `json:"children"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if len(req.Children) != 1 {
				t.Fatalf("attach req = %+v", req)
			}
			child := req.Children[0]
			// An SVG has to land in an "image" block: a "file" block is only
			// ever a download row, never rendered on the page.
			if child["type"] != "image" {
				t.Errorf("block type = %v, want image", child["type"])
			}
			value, _ := child["image"].(map[string]any)
			upload, _ := value["file_upload"].(map[string]any)
			if value["type"] != "file_upload" || upload["id"] != "fu-1" {
				t.Errorf("image block = %+v", value)
			}
			io.WriteString(w, `{"results":[{"id":"blk-1"}]}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	id, err := c.UploadFile(context.Background(), "page.svg", "image/svg+xml", strings.NewReader("<svg/>"))
	if err != nil {
		t.Fatal(err)
	}
	url, err := c.AttachFile(context.Background(), "11111111-2222-3333-4444-555555555555", id, "page.svg", "image/svg+xml", AttachEmbed)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://www.notion.so/11111111222233334444555555555555#blk1" {
		t.Errorf("block url = %q", url)
	}
	if !sawCreate || !sawSend || !sawAttach {
		t.Errorf("flow incomplete: create=%v send=%v attach=%v", sawCreate, sawSend, sawAttach)
	}
}

func TestUnauthorized(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"code":"unauthorized","message":"token revoked"}`)
	}))
	_, err := c.Search(context.Background(), "", 5)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v, want ErrUnauthorized", err)
	}
}

func TestAPIError(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"code":"validation_error","message":"bad page id"}`)
	}))
	_, err := c.AttachFile(context.Background(), "11111111-2222-3333-4444-555555555555", "y", "z", "application/pdf", AttachEmbed)
	if err == nil || !strings.Contains(err.Error(), "bad page id") {
		t.Fatalf("error should carry the API message, got %v", err)
	}
}

func TestBotInfo(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/me" {
			t.Errorf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{"bot":{"workspace_name":"Acme"}}`)
	}))
	ws, err := c.BotInfo(context.Background())
	if err != nil || ws != "Acme" {
		t.Fatalf("got %q, %v", ws, err)
	}
}

func TestBlockTypeFor(t *testing.T) {
	for _, tc := range []struct{ contentType, want string }{
		{"application/pdf", "pdf"},
		{"application/pdf; charset=binary", "pdf"},
		{"image/svg+xml", "image"},
		{"image/png", "image"},
		{"text/plain", "file"},
		{"", "file"},
	} {
		if got := BlockTypeFor(tc.contentType, AttachEmbed); got != tc.want {
			t.Errorf("BlockTypeFor(%q, embed) = %q, want %q", tc.contentType, got, tc.want)
		}
		// Asking for a plain file block overrides the content type: that is
		// the whole point of the embed/file choice.
		if got := BlockTypeFor(tc.contentType, AttachFileBlock); got != "file" {
			t.Errorf("BlockTypeFor(%q, file) = %q, want file", tc.contentType, got)
		}
	}
}

// A workspace that shared more pages than fit in one response must still be
// listed in full: the picker follows Notion's cursor pagination.
func TestSearchFollowsPagination(t *testing.T) {
	var starts []string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		cursor, _ := req["start_cursor"].(string)
		starts = append(starts, cursor)

		page := func(id string) string {
			return `{"object":"page","id":"` + id +
				`","properties":{"title":{"type":"title","title":[{"plain_text":"` + id + `"}]}}}`
		}
		switch cursor {
		case "":
			io.WriteString(w, `{"results":[`+page("a")+`,`+page("b")+`],"has_more":true,"next_cursor":"cur-2"}`)
		case "cur-2":
			io.WriteString(w, `{"results":[`+page("c")+`],"has_more":false,"next_cursor":null}`)
		default:
			t.Errorf("unexpected cursor %q", cursor)
		}
	}))

	pages, err := c.Search(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 {
		t.Fatalf("pages = %d (%+v), want all 3 across both responses", len(pages), pages)
	}
	if len(starts) != 2 || starts[0] != "" || starts[1] != "cur-2" {
		t.Errorf("cursors requested = %q", starts)
	}
}

// The limit is a hard cap: it must stop paging, not just trim one response.
func TestSearchStopsAtLimit(t *testing.T) {
	calls := 0
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if got := req["page_size"]; got != float64(2) {
			t.Errorf("page_size = %v, want the remaining 2", got)
		}
		io.WriteString(w, `{"results":[`+
			`{"object":"page","id":"a","properties":{}},`+
			`{"object":"page","id":"b","properties":{}}`+
			`],"has_more":true,"next_cursor":"more"}`)
	}))

	pages, err := c.Search(context.Background(), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || calls != 1 {
		t.Fatalf("pages = %d over %d calls, want 2 over 1", len(pages), calls)
	}
	if pages[0].Title != "Untitled" {
		t.Errorf("a page with no title should read Untitled, got %q", pages[0].Title)
	}
}

// Page ids reach the client over the local socket and are interpolated into
// the request path, so a value that is not an id must be refused before the
// user's token goes anywhere near it.
func TestRejectsNonIDPagePath(t *testing.T) {
	c := &Client{HTTP: http.DefaultClient, BaseURL: "https://api.notion.example", Token: "t"}
	for _, bad := range []string{"", "../../v1/users/me", "page-id-1", "11111111-2222-3333-4444-55555555555"} {
		if _, err := c.AttachFile(context.Background(), bad, "fu-1", "n.pdf", "application/pdf", AttachEmbed); !errors.Is(err, ErrBadID) {
			t.Errorf("AttachFile(%q): got %v, want ErrBadID", bad, err)
		}
		if _, err := c.ChildPageOrder(context.Background(), bad); !errors.Is(err, ErrBadID) {
			t.Errorf("ChildPageOrder(%q): got %v, want ErrBadID", bad, err)
		}
	}
}
