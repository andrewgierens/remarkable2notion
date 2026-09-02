// Package xochitl reads documents from the device's notebook store at
// /home/root/.local/share/remarkable/xochitl/.
package xochitl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultDir is the notebook store location on the device.
const DefaultDir = "/home/root/.local/share/remarkable/xochitl"

// Store reads notebooks from one directory.
type Store struct {
	Dir string
}

// Document is one notebook: its display name and ordered page ids.
type Document struct {
	UUID    string
	Name    string
	PageIDs []string
}

// content mirrors the parts of <uuid>.content we need. Newer firmware nests
// pages under cPages; older firmware has a flat pages array.
type content struct {
	CPages struct {
		Pages []struct {
			ID      string `json:"id"`
			Deleted *struct {
				Value int `json:"value"`
			} `json:"deleted"`
		} `json:"pages"`
	} `json:"cPages"`
	Pages []string `json:"pages"`
}

type metadata struct {
	VisibleName string `json:"visibleName"`
}

// Document loads a notebook by uuid.
func (s *Store) Document(uuid string) (*Document, error) {
	if strings.ContainsAny(uuid, "/\\.") {
		return nil, fmt.Errorf("xochitl: invalid document uuid %q", uuid)
	}
	doc := &Document{UUID: uuid}

	mb, err := os.ReadFile(filepath.Join(s.Dir, uuid+".metadata"))
	if err != nil {
		return nil, fmt.Errorf("xochitl: %w", err)
	}
	var md metadata
	if err := json.Unmarshal(mb, &md); err != nil {
		return nil, fmt.Errorf("xochitl: parse metadata: %w", err)
	}
	doc.Name = md.VisibleName

	cb, err := os.ReadFile(filepath.Join(s.Dir, uuid+".content"))
	if err != nil {
		return nil, fmt.Errorf("xochitl: %w", err)
	}
	var c content
	if err := json.Unmarshal(cb, &c); err != nil {
		return nil, fmt.Errorf("xochitl: parse content: %w", err)
	}
	if len(c.CPages.Pages) > 0 {
		for _, p := range c.CPages.Pages {
			if p.Deleted != nil && p.Deleted.Value > 0 {
				continue
			}
			doc.PageIDs = append(doc.PageIDs, p.ID)
		}
	} else {
		doc.PageIDs = c.Pages
	}
	if len(doc.PageIDs) == 0 {
		return nil, fmt.Errorf("xochitl: document %s has no pages", uuid)
	}
	return doc, nil
}

// PagePath returns the .rm file for one page of a document. It returns an
// error for a page id that would escape the document directory: ids come out
// of the .content file, which can arrive from the reMarkable cloud and is
// therefore not ours to trust.
func (s *Store) PagePath(uuid, pageID string) (string, error) {
	if pageID == "" || strings.ContainsAny(pageID, `/\`) || strings.Contains(pageID, "..") {
		return "", fmt.Errorf("xochitl: invalid page id %q", pageID)
	}
	return filepath.Join(s.Dir, uuid, pageID+".rm"), nil
}

// ParsePageRange resolves a 1-based page-range expression ("", "3", "1-3",
// "1,4-5") against a page count, returning 0-based indices in order.
func ParsePageRange(expr string, pageCount int) ([]int, error) {
	if strings.TrimSpace(expr) == "" {
		all := make([]int, pageCount)
		for i := range all {
			all[i] = i
		}
		return all, nil
	}
	var out []int
	seen := map[int]bool{}
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		lo, hi, ok := strings.Cut(part, "-")
		start, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("bad page range %q", part)
		}
		end := start
		if ok {
			if end, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("bad page range %q", part)
			}
		}
		if start < 1 || end > pageCount || start > end {
			return nil, fmt.Errorf("page range %q outside 1-%d", part, pageCount)
		}
		for i := start; i <= end; i++ {
			if !seen[i-1] {
				seen[i-1] = true
				out = append(out, i-1)
			}
		}
	}
	return out, nil
}
