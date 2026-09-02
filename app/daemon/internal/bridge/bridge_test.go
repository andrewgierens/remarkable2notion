package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/notion"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/pair"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/rm"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/socket"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/store"
	"github.com/andrewgierens/remarkable2notion/app/daemon/internal/xochitl"
)

// fakePage is one page in the fake's search results.
type fakePage struct {
	ID         string
	Title      string
	ParentPage string // parent page id, or "" for a database-parented page
}

// fakeNotion implements the API subset the bridge touches.
type fakeNotion struct {
	mu       sync.Mutex
	uploads  map[string][]byte // upload id → content
	attached []string          // "<page>:<type>:<upload>" in attach order
	blockSeq int
	fail401  bool
	// pages is what /v1/search returns; nil means the single default page.
	pages []fakePage
	// childOrder is the child-page order each page reports, keyed by page id.
	childOrder map[string][]string
	// fail401For rejects only requests carrying this bearer token, so a
	// test can revoke one connected account and leave the others working.
	fail401For string
}

func (f *fakeNotion) handler(t *testing.T) http.Handler {
	uploadSeq := 0
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		revoked := f.fail401
		if f.fail401For != "" && r.Header.Get("Authorization") == "Bearer "+f.fail401For {
			revoked = true
		}
		if revoked {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"unauthorized","message":"revoked"}`)
			return
		}
		switch {
		case r.URL.Path == "/v1/search":
			pages := f.pages
			if pages == nil {
				pages = []fakePage{{ID: "aaaaaaaa-0000-4000-8000-000000000001", Title: "Inbox"}}
			}
			results := make([]string, 0, len(pages))
			for _, p := range pages {
				parent := `{"type":"database_id","database_id":"db-1"}`
				if p.ParentPage != "" {
					parent = `{"type":"page_id","page_id":"` + p.ParentPage + `"}`
				}
				results = append(results, `{"object":"page","id":"`+p.ID+
					`","parent":`+parent+
					`,"properties":{"title":{"type":"title","title":[{"plain_text":"`+p.Title+`"}]}}}`)
			}
			io.WriteString(w, `{"results":[`+strings.Join(results, ",")+`]}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			blocks := make([]string, 0)
			for _, child := range f.childOrder[id] {
				blocks = append(blocks, `{"id":"`+child+`","type":"child_page"}`)
			}
			io.WriteString(w, `{"results":[`+strings.Join(blocks, ",")+`]}`)
		case r.URL.Path == "/v1/file_uploads":
			uploadSeq++
			fmt.Fprintf(w, `{"id":"fu-%d"}`, uploadSeq)
		case strings.HasPrefix(r.URL.Path, "/v1/file_uploads/") && strings.HasSuffix(r.URL.Path, "/send"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/file_uploads/"), "/send")
			r.ParseMultipartForm(32 << 20)
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Errorf("multipart field 'file' missing: %v", err)
				w.WriteHeader(400)
				return
			}
			body, _ := io.ReadAll(file)
			f.uploads[id] = body
			io.WriteString(w, `{"status":"uploaded"}`)
		case strings.HasSuffix(r.URL.Path, "/children"):
			page := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			var req struct {
				Children []map[string]any `json:"children"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			ids := []string{}
			for _, child := range req.Children {
				blockType, _ := child["type"].(string)
				value, _ := child[blockType].(map[string]any)
				up, _ := value["file_upload"].(map[string]any)
				uploadID, _ := up["id"].(string)
				f.blockSeq++
				f.attached = append(f.attached, page+":"+blockType+":"+uploadID)
				ids = append(ids, fmt.Sprintf(`{"id":"blk-%d"}`, f.blockSeq))
			}
			io.WriteString(w, `{"results":[`+strings.Join(ids, ",")+`]}`)
		case r.URL.Path == "/v1/users/me":
			io.WriteString(w, `{"bot":{"workspace_name":"Acme"}}`)
		default:
			t.Errorf("fakeNotion: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
}

type env struct {
	bridge  *Bridge
	sock    string
	notion  *fakeNotion
	broker  *brokerState
	confDir string
}

type brokerState struct {
	completed bool
}

func setup(t *testing.T) *env {
	t.Helper()
	confDir := t.TempDir()

	fn := &fakeNotion{uploads: map[string][]byte{}}
	notionSrv := httptest.NewServer(fn.handler(t))
	t.Cleanup(notionSrv.Close)

	bs := &brokerState{}
	var brokerOrigin string
	brokerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/pair":
			// The user code in the verification URL is a separate secret from
			// the device code the daemon polls with.
			fmt.Fprintf(w, `{"device_code":"dc-1","verification_url":%q}`, brokerOrigin+"/go/uc-1")
		case r.URL.Path == "/pair/dc-1":
			if bs.completed {
				io.WriteString(w, `{"state":"ok","access_token":"tok-abc","workspace":"Acme"}`)
			} else {
				io.WriteString(w, `{"state":"pending"}`)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(brokerSrv.Close)
	brokerOrigin = brokerSrv.URL

	docsDir := t.TempDir()
	writePage := func(uuid, pageID string, lines []rm.Line) {
		os.MkdirAll(filepath.Join(docsDir, uuid), 0o755)
		os.WriteFile(filepath.Join(docsDir, uuid, pageID+".rm"), rm.BuildSimplePage(lines, nil), 0o644)
	}
	os.WriteFile(filepath.Join(docsDir, "doc1.metadata"), []byte(`{"visibleName":"Sketch: ideas"}`), 0o644)
	os.WriteFile(filepath.Join(docsDir, "doc1.content"), []byte(`{"cPages":{"pages":[{"id":"pg-a"},{"id":"pg-b"}]}}`), 0o644)
	line := rm.Line{Tool: rm.PenFineliner2, ThicknessScale: 2, Points: []rm.Point{{X: 0, Y: 0, Width: 2}, {X: 10, Y: 10, Width: 2}}}
	writePage("doc1", "pg-a", []rm.Line{line})
	writePage("doc1", "pg-b", []rm.Line{line})

	st, err := store.New(confDir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(st, &xochitl.Store{Dir: docsDir}, pair.New(brokerSrv.URL), filepath.Join(confDir, "qr.png"))
	b.NewNotion = func(token string) *notion.Client {
		c := notion.New(token)
		c.BaseURL = notionSrv.URL
		return c
	}

	// Not t.TempDir(): its path is derived from the test name and unix
	// socket paths cap at ~104 bytes, so long test names fail to bind.
	sockDir, err := os.MkdirTemp("/tmp", "rmn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "bridge.sock")
	srv := socket.NewServer(sockPath)
	b.RegisterAll(srv)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	return &env{bridge: b, sock: sockPath, notion: fn, broker: bs, confDir: confDir}
}

// call sends one request over the real unix socket.
func (e *env) call(t *testing.T, method string, params any) (map[string]any, string) {
	t.Helper()
	conn, err := net.Dial("unix", e.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := map[string]any{"method": method}
	if params != nil {
		req["params"] = params
	}
	line, _ := json.Marshal(req)
	conn.Write(append(line, '\n'))
	respLine, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  string         `json:"error"`
	}
	if err := json.Unmarshal(respLine, &resp); err != nil {
		t.Fatalf("bad response %q: %v", respLine, err)
	}
	return resp.Result, resp.Error
}

func TestFullPairAndSendFlow(t *testing.T) {
	e := setup(t)

	// Unauthed status.
	res, errMsg := e.call(t, "status", nil)
	if errMsg != "" || res["authed"] != false {
		t.Fatalf("status = %v / %s", res, errMsg)
	}

	// Targets before pairing must prompt, not crash.
	if _, errMsg = e.call(t, "targets.list", nil); !strings.Contains(errMsg, "no Notion account connected") {
		t.Fatalf("unpaired targets.list error = %q", errMsg)
	}

	// Pair: start renders a QR, poll goes pending → ok and stores the token.
	res, errMsg = e.call(t, "pair.start", nil)
	if errMsg != "" || res["device_code"] != "dc-1" {
		t.Fatalf("pair.start = %v / %s", res, errMsg)
	}
	if _, err := os.Stat(res["qr_png_path"].(string)); err != nil {
		t.Fatalf("QR file: %v", err)
	}
	res, _ = e.call(t, "pair.poll", map[string]string{"device_code": "dc-1"})
	if res["state"] != "pending" {
		t.Fatalf("poll = %v", res)
	}
	e.broker.completed = true
	res, errMsg = e.call(t, "pair.poll", map[string]string{"device_code": "dc-1"})
	if errMsg != "" || res["state"] != "ok" || res["workspace"] != "Acme" {
		t.Fatalf("poll after consent = %v / %s", res, errMsg)
	}

	res, _ = e.call(t, "status", nil)
	if res["authed"] != true || res["workspace"] != "Acme" {
		t.Fatalf("paired status = %v", res)
	}

	// Targets now come from search.
	res, errMsg = e.call(t, "targets.list", map[string]string{"query": "in"})
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	accounts := res["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("targets = %v, want one account group", res)
	}
	group := accounts[0].(map[string]any)
	if group["workspace"] != "Acme" || group["account_id"] == "" {
		t.Fatalf("account group = %v", group)
	}
	pages := group["pages"].([]any)
	if len(pages) != 1 || pages[0].(map[string]any)["title"] != "Inbox" {
		t.Fatalf("targets = %v", res)
	}

	// Send both pages as PDF: one upload, one attach.
	res, errMsg = e.call(t, "send", map[string]string{
		"doc_uuid": "doc1", "page_range": "", "format": "pdf",
		"target_page_id": "aaaaaaaa-0000-4000-8000-000000000001", "target_title": "Inbox",
	})
	if errMsg != "" || res["ok"] != true {
		t.Fatalf("send pdf = %v / %s", res, errMsg)
	}
	if len(e.notion.uploads) != 1 {
		t.Fatalf("pdf should be one upload, got %d", len(e.notion.uploads))
	}
	for _, data := range e.notion.uploads {
		if !strings.HasPrefix(string(data), "%PDF-") {
			t.Error("upload is not a PDF")
		}
	}
	if res["block_url"] != "https://www.notion.so/aaaaaaaa000040008000000000000001#blk1" {
		t.Errorf("block_url = %v", res["block_url"])
	}

	// Send page 2 as SVG.
	res, errMsg = e.call(t, "send", map[string]string{
		"doc_uuid": "doc1", "page_range": "2", "format": "svg",
		"target_page_id": "aaaaaaaa-0000-4000-8000-000000000001", "target_title": "Inbox",
	})
	if errMsg != "" || res["ok"] != true {
		t.Fatalf("send svg = %v / %s", res, errMsg)
	}
	if len(e.notion.uploads) != 2 {
		t.Fatalf("expected a second upload, got %d", len(e.notion.uploads))
	}

	// Logout drops the token.
	if _, errMsg = e.call(t, "logout", nil); errMsg != "" {
		t.Fatal(errMsg)
	}
	res, _ = e.call(t, "status", nil)
	if res["authed"] != false {
		t.Fatal("still authed after logout")
	}
}

func TestRevokedTokenDropsPairing(t *testing.T) {
	e := setup(t)
	acc, err := e.bridge.Store.AddAccount("tok-abc", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	e.notion.fail401 = true

	_, errMsg := e.call(t, "targets.list", nil)
	if !strings.Contains(errMsg, "revoked") {
		t.Fatalf("error = %q", errMsg)
	}
	if e.bridge.Store.TokenFor(acc.ID) != "" {
		t.Fatal("401 must forget that account")
	}
}

// A revoked account must not take the other one down with it: the picker
// still has to list the workspace that still works.
func TestRevokedAccountIsolated(t *testing.T) {
	e := setup(t)
	bad, _ := e.bridge.Store.AddAccount("tok-bad", "Acme")
	good, _ := e.bridge.Store.AddAccount("tok-good", "Home")
	e.notion.fail401For = "tok-bad"

	res, errMsg := e.call(t, "targets.list", nil)
	if errMsg != "" {
		t.Fatalf("one bad account must not fail the call: %q", errMsg)
	}
	groups := res["accounts"].([]any)
	if len(groups) != 2 {
		t.Fatalf("groups = %v", groups)
	}
	badGroup, goodGroup := groups[0].(map[string]any), groups[1].(map[string]any)
	if badGroup["error"] == nil || !strings.Contains(badGroup["error"].(string), "revoked") {
		t.Errorf("revoked group should carry its error: %v", badGroup)
	}
	if len(goodGroup["pages"].([]any)) != 1 {
		t.Errorf("healthy group should still list pages: %v", goodGroup)
	}
	if e.bridge.Store.TokenFor(bad.ID) != "" {
		t.Error("revoked account should be forgotten")
	}
	if e.bridge.Store.TokenFor(good.ID) != "tok-good" {
		t.Error("healthy account must survive")
	}
}

// With two accounts connected, a send that does not name one is ambiguous
// and must be refused rather than guessing a workspace.
func TestSendNeedsAccountID(t *testing.T) {
	e := setup(t)
	e.bridge.Store.AddAccount("tok-a", "Acme")
	home, _ := e.bridge.Store.AddAccount("tok-b", "Home")

	_, errMsg := e.call(t, "send", map[string]string{
		"doc_uuid": "doc1", "format": "pdf", "target_page_id": "aaaaaaaa-0000-4000-8000-000000000001",
	})
	if !strings.Contains(errMsg, "more than one account") {
		t.Fatalf("ambiguous send error = %q", errMsg)
	}

	res, errMsg := e.call(t, "send", map[string]string{
		"doc_uuid": "doc1", "format": "pdf", "target_page_id": "aaaaaaaa-0000-4000-8000-000000000001",
		"account_id": home.ID,
	})
	if errMsg != "" || res["ok"] != true {
		t.Fatalf("named send = %v / %s", res, errMsg)
	}
}

// Removing one account leaves the other connected.
func TestLogoutOneAccount(t *testing.T) {
	e := setup(t)
	work, _ := e.bridge.Store.AddAccount("tok-a", "Acme")
	e.bridge.Store.AddAccount("tok-b", "Home")

	if _, errMsg := e.call(t, "logout", map[string]string{"account_id": work.ID}); errMsg != "" {
		t.Fatal(errMsg)
	}
	res, _ := e.call(t, "status", nil)
	accounts := res["accounts"].([]any)
	if res["authed"] != true || len(accounts) != 1 {
		t.Fatalf("status after single logout = %v", res)
	}
	if accounts[0].(map[string]any)["workspace"] != "Home" {
		t.Errorf("wrong account survived: %v", accounts)
	}

	// A bare logout still clears everything.
	if _, errMsg := e.call(t, "logout", nil); errMsg != "" {
		t.Fatal(errMsg)
	}
	res, _ = e.call(t, "status", nil)
	if res["authed"] != false {
		t.Fatal("bare logout should drop every account")
	}
}

func TestSendValidation(t *testing.T) {
	e := setup(t)
	e.bridge.Store.AddAccount("tok-abc", "Acme")

	_, errMsg := e.call(t, "send", map[string]string{"doc_uuid": "doc1", "format": "png", "target_page_id": "aaaaaaaa-0000-4000-8000-000000000001"})
	if !strings.Contains(errMsg, "unknown format") {
		t.Errorf("bad format error = %q", errMsg)
	}
	_, errMsg = e.call(t, "send", map[string]string{"doc_uuid": "doc1", "format": "svg", "target_page_id": "aaaaaaaa-0000-4000-8000-000000000001", "page_range": "9"})
	if !strings.Contains(errMsg, "outside") {
		t.Errorf("bad range error = %q", errMsg)
	}
	_, errMsg = e.call(t, "send", map[string]string{"doc_uuid": "missing", "format": "svg", "target_page_id": "aaaaaaaa-0000-4000-8000-000000000001"})
	if errMsg == "" {
		t.Error("missing document must error")
	}
}

func TestAttachAs(t *testing.T) {
	e := setup(t)
	e.bridge.Store.AddAccount("tok-a", "Acme")

	_, errMsg := e.call(t, "send", map[string]string{
		"doc_uuid": "doc1", "format": "pdf", "target_page_id": "aaaaaaaa-0000-4000-8000-000000000001", "attach_as": "inline",
	})
	if !strings.Contains(errMsg, "unknown attach_as") {
		t.Errorf("error = %q", errMsg)
	}

	// Embedding puts a PDF in a pdf block so it renders on the page; asking
	// for a file block instead gives a download row.
	for _, tc := range []struct{ attachAs, want string }{
		{"", "pdf"}, {"embed", "pdf"}, {"file", "file"},
	} {
		e.notion.attached = nil
		if _, errMsg := e.call(t, "send", map[string]string{
			"doc_uuid": "doc1", "format": "pdf", "page_range": "1",
			"target_page_id": "aaaaaaaa-0000-4000-8000-000000000001", "attach_as": tc.attachAs,
		}); errMsg != "" {
			t.Fatalf("attach_as %q: %s", tc.attachAs, errMsg)
		}
		if len(e.notion.attached) != 1 || !strings.Contains(e.notion.attached[0], ":"+tc.want+":") {
			t.Errorf("attach_as %q attached %v, want a %s block", tc.attachAs, e.notion.attached, tc.want)
		}
	}
}

// targets.refresh picks up a workspace rename and returns a fresh list.
func TestTargetsRefreshUpdatesWorkspaceName(t *testing.T) {
	e := setup(t)
	acc, _ := e.bridge.Store.AddAccount("tok-a", "Old Name")

	res, errMsg := e.call(t, "targets.refresh", nil)
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	// The fake reports the workspace as "Acme".
	if got := e.bridge.Store.Accounts(); len(got) != 1 || got[0].Workspace != "Acme" {
		t.Fatalf("workspace not refreshed: %+v", got)
	}
	group := res["accounts"].([]any)[0].(map[string]any)
	if group["workspace"] != "Acme" || group["account_id"] != acc.ID {
		t.Errorf("refreshed group = %v", group)
	}
	if len(group["pages"].([]any)) != 1 {
		t.Errorf("refresh should return pages too: %v", group)
	}
}

// A page whose parent was also shared must appear directly under it, and a
// page whose parent was not shared stands on its own.
func TestArrangePagesNesting(t *testing.T) {
	e := setup(t)
	e.notion.pages = []fakePage{
		{ID: "root-b", Title: "Beta"},
		{ID: "kid-2", Title: "Zulu", ParentPage: "root-b"},
		{ID: "kid-1", Title: "Alpha kid", ParentPage: "root-b"},
		{ID: "root-a", Title: "alpha"},
		{ID: "orphan", Title: "Orphan", ParentPage: "not-shared"},
	}
	e.bridge.Store.AddAccount("tok-a", "Acme")

	res, errMsg := e.call(t, "targets.list", nil)
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	pages := res["accounts"].([]any)[0].(map[string]any)["pages"].([]any)

	type row struct {
		id    string
		depth int
	}
	var got []row
	for _, p := range pages {
		m := p.(map[string]any)
		got = append(got, row{m["id"].(string), int(m["depth"].(float64))})
	}
	// Roots by title, case-insensitively: "alpha", "Beta", "Orphan"; the
	// children of Beta follow it, indented.
	want := []row{{"root-a", 0}, {"root-b", 0}, {"kid-1", 1}, {"kid-2", 1}, {"orphan", 0}}
	if len(got) != len(want) {
		t.Fatalf("pages = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v (all: %+v)", i, got[i], want[i], got)
		}
	}
}

// When the parent page reports its own child order, that wins over titles —
// it is the order the user sees in Notion.
func TestArrangePagesUsesParentOrder(t *testing.T) {
	e := setup(t)
	e.notion.pages = []fakePage{
		{ID: "bbbbbbbb-0000-4000-8000-000000000000", Title: "Root"},
		{ID: "cccccccc-0000-4000-8000-00000000000a", Title: "Alpha", ParentPage: "bbbbbbbb-0000-4000-8000-000000000000"},
		{ID: "cccccccc-0000-4000-8000-00000000000f", Title: "Zulu", ParentPage: "bbbbbbbb-0000-4000-8000-000000000000"},
	}
	// Notion lists Zulu first on the page.
	e.notion.childOrder = map[string][]string{"bbbbbbbb-0000-4000-8000-000000000000": {"cccccccc-0000-4000-8000-00000000000f", "cccccccc-0000-4000-8000-00000000000a"}}
	e.bridge.Store.AddAccount("tok-a", "Acme")

	res, _ := e.call(t, "targets.list", nil)
	pages := res["accounts"].([]any)[0].(map[string]any)["pages"].([]any)
	var ids []string
	for _, p := range pages {
		ids = append(ids, p.(map[string]any)["id"].(string))
	}
	want := []string{"bbbbbbbb-0000-4000-8000-000000000000", "cccccccc-0000-4000-8000-00000000000f", "cccccccc-0000-4000-8000-00000000000a"}
	if len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}
