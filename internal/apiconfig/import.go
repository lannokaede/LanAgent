// Package apiconfig handles user-facing API setup without logging credentials.
package apiconfig

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lannokaede/LanAgent/internal/config"
	"go.yaml.in/yaml/v3"
)

func field(name, kind string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.NewReplacer(" ", "", "_", "", "-", "").Replace(name)
	prefix := "llm"
	if kind == "embedding" {
		prefix = "embedding"
	}
	name = strings.TrimPrefix(name, prefix)
	switch name {
	case "baseurl", "apibase", "openaiapibase", "openaibaseurl", "url", "接口地址":
		return "base_url"
	case "apikey", "openaiapikey", "key", "密钥":
		return "api_key"
	case "model", "openaimodel", "modelname", "模型", "模型名", "模型名称":
		return "model"
	case "dimensions", "dimension", "维度":
		return "dimensions"
	}
	return ""
}
func assign(endpoint *config.APIEndpoint, key, value string) error {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "\"'")
	switch key {
	case "base_url":
		endpoint.BaseURL = value
	case "api_key":
		endpoint.APIKey = value
	case "model":
		endpoint.Model = value
	case "dimensions":
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 {
			return errors.New("embedding dimensions must be positive")
		}
		endpoint.Dimensions = n
	}
	return nil
}

// ParseImport supports JSON/YAML, .env, and provider messages with a value on
// the next line. Error strings deliberately exclude imported content.
func ParseImport(text, kind string) (config.APIEndpoint, error) {
	var endpoint config.APIEndpoint
	text = strings.TrimPrefix(text, "\ufeff")
	if len(text) > 64<<10 {
		return endpoint, errors.New("API import exceeds 64 KiB")
	}
	var values map[string]any
	if yaml.Unmarshal([]byte(text), &values) == nil && len(values) > 0 {
		nested := "llm"
		if kind == "embedding" {
			nested = "embedding"
		}
		if selected, ok := values[nested].(map[string]any); ok {
			values = selected
		} else {
			if _, ok := values["llm"]; ok {
				return endpoint, errors.New("no matching API section in import")
			}
			if _, ok := values["embedding"]; ok {
				return endpoint, errors.New("no matching API section in import")
			}
		}
		found := false
		for key, value := range values {
			name := field(key, kind)
			if name == "" {
				continue
			}
			if name != "dimensions" {
				if _, ok := value.(string); !ok {
					return endpoint, errors.New("API URL, key and model must be strings")
				}
			}
			if e := assign(&endpoint, name, fmt.Sprint(value)); e != nil {
				return endpoint, e
			}
			found = true
		}
		if found {
			return endpoint, nil
		}
	}
	pending := ""
	found := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "```") {
			continue
		}
		if pending != "" {
			if e := assign(&endpoint, pending, line); e != nil {
				return endpoint, e
			}
			pending = ""
			found = true
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		line = strings.ReplaceAll(line, "：", ":")
		sep := strings.IndexAny(line, "=:")
		if sep < 0 {
			continue
		}
		name := field(line[:sep], kind)
		if name == "" {
			continue
		}
		value := strings.TrimSpace(line[sep+1:])
		if value == "" {
			pending = name
			continue
		}
		if e := assign(&endpoint, name, value); e != nil {
			return endpoint, e
		}
		found = true
	}
	if !found || pending != "" {
		return endpoint, errors.New("import must contain Base URL, API Key and optionally model; incomplete value")
	}
	return endpoint, nil
}
