package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// APIEndpoint contains credentials and must never be included in logs.
type APIEndpoint struct {
	BaseURL    string `json:"base_url,omitempty"`
	Model      string `json:"model,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
	Dimensions int    `json:"dimensions,omitempty"`
}
type APISettings struct {
	Version   int         `json:"version"`
	Chat      APIEndpoint `json:"llm"`
	Embedding APIEndpoint `json:"embedding"`
}

func APISettingsPath() (string, error) {
	if path := os.Getenv("LANAGENT_API_CONFIG"); path != "" {
		return filepath.Abs(path)
	}
	dir, e := os.UserConfigDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(dir, "lanagent", "api.local.json"), nil
}
func ReadAPISettings(path string) (APISettings, error) {
	settings := APISettings{Version: 1}
	f, e := os.Open(path)
	if errors.Is(e, os.ErrNotExist) {
		return settings, nil
	}
	if e != nil {
		return settings, fmt.Errorf("cannot open local API settings")
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if e != nil || len(b) > 64<<10 {
		return settings, errors.New("cannot read local API settings")
	}
	if json.Unmarshal(b, &settings) != nil || settings.Version != 1 {
		return APISettings{}, errors.New("invalid local API settings (expected version 1); repair or back up this file before reconfiguring")
	}
	return settings, nil
}
func WriteAPISettings(path string, settings APISettings) error {
	settings.Version = 1
	data, e := json.MarshalIndent(settings, "", "  ")
	if e != nil {
		return errors.New("cannot encode local API settings")
	}
	dir := filepath.Dir(path)
	if e = os.MkdirAll(dir, 0700); e != nil {
		return fmt.Errorf("cannot create local API configuration directory")
	}
	f, e := os.CreateTemp(dir, ".api-*.tmp")
	if e != nil {
		return errors.New("cannot create local API configuration")
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if errors.Join(writeErr, syncErr, closeErr) != nil {
		return errors.New("cannot save local API configuration")
	}
	if os.Rename(f.Name(), path) != nil {
		return errors.New("cannot replace local API configuration")
	}
	return nil
}

// NormalizeAPIURL preserves provider prefixes such as /api/v1 and accepts a
// pasted Chat Completions endpoint without appending the route a second time.
func NormalizeAPIURL(raw string) (string, error) {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("Base URL must be an HTTP(S) URL without credentials, query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	for _, suffix := range []string{"/chat/completions", "/embeddings", "/models"} {
		if strings.HasSuffix(u.Path, suffix) {
			u.Path = strings.TrimSuffix(u.Path, suffix)
			u.RawPath = ""
			break
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}
func endpointMap(e APIEndpoint) map[string]any {
	m := map[string]any{}
	if e.BaseURL != "" {
		m["base_url"] = e.BaseURL
	}
	if e.Model != "" {
		m["model"] = e.Model
	}
	if e.APIKey != "" {
		m["api_key"] = e.APIKey
	}
	if e.Dimensions > 0 {
		m["dimensions"] = e.Dimensions
	}
	return m
}
func (s APISettings) defaults() map[string]any {
	return map[string]any{"llm": endpointMap(s.Chat), "embedding": endpointMap(s.Embedding)}
}
