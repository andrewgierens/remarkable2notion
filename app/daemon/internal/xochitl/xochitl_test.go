package xochitl

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeDoc(t *testing.T, dir, uuid, metadata, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, uuid+".metadata"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, uuid+".content"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentNewFormat(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, "doc1",
		`{"visibleName":"My Notes"}`,
		`{"cPages":{"pages":[
			{"id":"page-a"},
			{"id":"page-b","deleted":{"value":1}},
			{"id":"page-c"}
		]}}`)

	s := &Store{Dir: dir}
	doc, err := s.Document("doc1")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Name != "My Notes" {
		t.Errorf("name = %q", doc.Name)
	}
	if !reflect.DeepEqual(doc.PageIDs, []string{"page-a", "page-c"}) {
		t.Errorf("pages = %v (deleted page must be filtered)", doc.PageIDs)
	}
	got, err := s.PagePath("doc1", "page-a")
	if err != nil || got != filepath.Join(dir, "doc1", "page-a.rm") {
		t.Errorf("page path = %s, %v", got, err)
	}
}

func TestDocumentOldFormat(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, "doc2", `{"visibleName":"Old"}`, `{"pages":["p1","p2"]}`)
	doc, err := (&Store{Dir: dir}).Document("doc2")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.PageIDs, []string{"p1", "p2"}) {
		t.Errorf("pages = %v", doc.PageIDs)
	}
}

func TestDocumentRejectsTraversal(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	for _, uuid := range []string{"../etc/passwd", "a/b", "x.y"} {
		if _, err := s.Document(uuid); err == nil {
			t.Errorf("uuid %q should be rejected", uuid)
		}
	}
}

func TestParsePageRange(t *testing.T) {
	cases := []struct {
		expr string
		n    int
		want []int
		err  bool
	}{
		{"", 3, []int{0, 1, 2}, false},
		{"2", 3, []int{1}, false},
		{"1-3", 5, []int{0, 1, 2}, false},
		{"1,3-4", 5, []int{0, 2, 3}, false},
		{"2,2,1-2", 3, []int{1, 0}, false},
		{"0", 3, nil, true},
		{"4", 3, nil, true},
		{"3-1", 3, nil, true},
		{"x", 3, nil, true},
	}
	for _, c := range cases {
		got, err := ParsePageRange(c.expr, c.n)
		if c.err {
			if err == nil {
				t.Errorf("%q: expected error", c.expr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.expr, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %v, want %v", c.expr, got, c.want)
		}
	}
}

// Page ids come out of the .content file, which can arrive from the reMarkable
// cloud, so one that would escape the document directory must be refused.
func TestPagePathRejectsTraversal(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	for _, bad := range []string{"", "..", "../../../etc/passwd", "a/b", `a\b`, `..\x`} {
		if _, err := s.PagePath("doc1", bad); err == nil {
			t.Errorf("PagePath(%q) was accepted", bad)
		}
	}
}
