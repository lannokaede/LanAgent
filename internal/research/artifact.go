package research

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type tempDir struct{ path string }

func osTemp(parent string) (tempDir, error) {
	p, e := os.MkdirTemp(parent, "fetch-")
	return tempDir{p}, e
}
func (d tempDir) close() { os.RemoveAll(d.path) }
func (d tempDir) read(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.path, "corpus", filepath.Base(name)))
}

type ArtifactPage struct {
	ArtifactID string `json:"artifact_id"`
	Content    string `json:"content"`
	NextOffset int    `json:"next_offset"`
	Truncated  bool   `json:"truncated"`
	SHA256     string `json:"sha256"`
}

func (s *Service) ReadArtifact(id string, offset int) (ArtifactPage, error) {
	b, e := s.body(id)
	if e != nil {
		return ArtifactPage{}, e
	}
	if offset < 0 || offset > len(b) || offset < len(b) && !utf8.RuneStart(b[offset]) {
		return ArtifactPage{}, errors.New("invalid UTF-8 byte offset")
	}
	end := min(len(b), offset+2048)
	for end < len(b) && !utf8.RuneStart(b[end]) {
		end--
	}
	page := ArtifactPage{id, string(b[offset:end]), end, end < len(b), hash(b)}
	// Keep the serialized observation below the Harness compaction threshold,
	// including JSON escaping, so continuation never silently skips a preview.
	for {
		encoded, err := json.Marshal(page)
		if err != nil {
			return ArtifactPage{}, err
		}
		if len(encoded) <= 3500 {
			return page, nil
		}
		end--
		for end > offset && !utf8.RuneStart(b[end]) {
			end--
		}
		page.Content = string(b[offset:end])
		page.NextOffset = end
		page.Truncated = end < len(b)
	}
}

type ArtifactMatch struct {
	Offset  int    `json:"offset"`
	Excerpt string `json:"excerpt"`
}

func (s *Service) SearchArtifact(id, q string) ([]ArtifactMatch, error) {
	if q == "" || len(q) > 256 {
		return nil, errors.New("query requires 1..256 bytes")
	}
	b, e := s.body(id)
	if e != nil {
		return nil, e
	}
	text := string(b)
	out := []ArtifactMatch{}
	for off := 0; off < len(b) && len(out) < 10; {
		at := strings.Index(text[off:], q)
		if at < 0 {
			break
		}
		at += off
		start := max(0, at-120)
		for start < at && !utf8.RuneStart(b[start]) {
			start++
		}
		end := min(len(b), at+len(q)+240)
		for end < len(b) && !utf8.RuneStart(b[end]) {
			end--
		}
		out = append(out, ArtifactMatch{at, text[start:end]})
		encoded, _ := json.Marshal(out)
		if len(encoded) > 3500 {
			out = out[:len(out)-1]
			break
		}
		off = at + len(q)
	}
	return out, nil
}
