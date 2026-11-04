package personal

import (
	"encoding/json"
	"errors"
	"github.com/lannokaede/LanAgent/internal/llm"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Launch stores only non-secret startup choices, never model keys or MCP tokens.
type Launch struct {
	Config    string        `json:"config"`
	Workspace string        `json:"workspace"`
	MCP       string        `json:"mcp,omitempty"`
	Exercise  string        `json:"exercise,omitempty"`
	Write     bool          `json:"write"`
	Commands  bool          `json:"commands"`
	Summary   bool          `json:"summary"`
	Rounds    int           `json:"rounds"`
	Timeout   time.Duration `json:"timeout"`
}
type SessionInfo struct {
	ID, Title, Workspace, Status string
	Updated                      time.Time
	Launch                       *Launch
}

func (s *Session) SaveLaunch(p Launch) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	return s.writeAtomic(s.ID+".launch.json", b)
}
func List(dir string) ([]SessionInfo, error) {
	root, e := os.OpenRoot(dir)
	if errors.Is(e, os.ErrNotExist) {
		return []SessionInfo{}, nil
	}
	if e != nil {
		return nil, e
	}
	defer root.Close()
	d, e := root.Open(".")
	if e != nil {
		return nil, e
	}
	defer d.Close()
	entries, e := d.ReadDir(10001)
	if e != nil && e != io.EOF {
		return nil, e
	}
	if len(entries) > 10000 {
		return nil, errors.New("session directory exceeds catalog limit")
	}
	ids := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if strings.HasSuffix(id, ".pending") {
			id = strings.TrimSuffix(id, ".pending")
		}
		if validID.MatchString(id) {
			ids[id] = true
		}
	}
	out := []SessionInfo{}
	for id := range ids {
		info := SessionInfo{ID: id, Title: "新会话", Status: "已保存"}
		var snap snapshot
		var pending pendingState
		good := false
		if modified, e := catalogJSON(root, id+".json", &snap, maxSessionBytes); e == nil && snap.Version == 1 {
			good = true
			info.Workspace = snap.Workspace
			info.Updated = modified
			info.Title = conversationTitle(snap.Messages)
		}
		if modified, e := catalogJSON(root, id+".pending.json", &pending, maxSessionBytes); e == nil && pending.Checkpoint.Version == 1 && pending.Checkpoint.RunID != snap.LastRun {
			good = true
			info.Workspace = pending.Workspace
			info.Status = "未完成"
			if pending.Checkpoint.Running != "" {
				info.Status = "需核对"
			}
			if modified.After(info.Updated) {
				info.Updated = modified
			}
			if info.Title == "新会话" {
				info.Title = conversationTitle(pending.Checkpoint.Messages)
			}
		}
		if !good {
			continue
		}
		var profile Launch
		if _, e := catalogJSON(root, id+".launch.json", &profile, 32<<10); e == nil && profile.Workspace == info.Workspace && profile.Rounds > 0 && profile.Timeout > 0 {
			info.Launch = &profile
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Updated.Equal(out[j].Updated) {
			return out[i].ID < out[j].ID
		}
		return out[i].Updated.After(out[j].Updated)
	})
	return out, nil
}
func catalogJSON(root *os.Root, name string, out any, limit int64) (time.Time, error) {
	stat, e := root.Lstat(name)
	if e != nil {
		return time.Time{}, e
	}
	if !stat.Mode().IsRegular() {
		return time.Time{}, errors.New("catalog entry must be regular")
	}
	f, e := root.Open(name)
	if e != nil {
		return time.Time{}, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(b)) > limit {
		return time.Time{}, errors.New("catalog entry exceeds limit")
	}
	if e = json.Unmarshal(b, out); e != nil {
		return time.Time{}, e
	}
	return stat.ModTime(), nil
}

func conversationTitle(messages []llm.Message) string {
	for _, m := range messages {
		if m.Role == llm.RoleUser {
			runes := []rune(strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return ' '
				}
				return r
			}, m.Content))
			if len(runes) > 48 {
				return string(runes[:48]) + "…"
			}
			return string(runes)
		}
	}
	return "新会话"
}
