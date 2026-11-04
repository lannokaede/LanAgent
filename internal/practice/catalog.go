package practice

import (
	"encoding/json"
	"fmt"
	assets "github.com/lannokaede/LanAgent/examples/practice"
	"io/fs"
	"sort"
	"strings"
)

type Catalog struct{ entries map[string]Exercise }

func NewCatalog() (*Catalog, error) {
	c := &Catalog{entries: map[string]Exercise{}}
	err := fs.WalkDir(assets.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, "/exercise.json") {
			return nil
		}
		b, err := assets.Files.ReadFile(path)
		if err != nil {
			return err
		}
		var e Exercise
		if err = json.Unmarshal(b, &e); err != nil {
			return err
		}
		dir := strings.TrimSuffix(path, "exercise.json")
		if dir != e.ID+"/"+e.Version+"/" {
			return fmt.Errorf("exercise identity mismatch")
		}
		starter, err := assets.Files.ReadFile(dir + "starter.go.txt")
		if err != nil {
			return err
		}
		tests, err := assets.Files.ReadFile(dir + "tests.go.txt")
		if err != nil {
			return err
		}
		e.StarterCode, e.Tests = string(starter), string(tests)
		c.entries[e.ID+"/"+e.Version] = e
		return nil
	})
	return c, err
}
func (c *Catalog) Get(id, version string) (Exercise, error) {
	e, ok := c.entries[id+"/"+version]
	if !ok {
		return Exercise{}, ErrNotFound
	}
	return e, nil
}
func (c *Catalog) List() []Exercise {
	out := make([]Exercise, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID+out[i].Version < out[j].ID+out[j].Version })
	return out
}
