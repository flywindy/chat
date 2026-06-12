package drive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfig_LoadBaseURLs_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseurls.json")
	if err := os.WriteFile(path, []byte(`{"site-a":"https://a.example.com","site-b":"https://b.example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{BaseURLConfigPath: path}
	c.LoadBaseURLs()
	if c.BaseURLMap["site-a"] != "https://a.example.com" {
		t.Fatalf("got %q", c.BaseURLMap["site-a"])
	}
	if c.BaseURLMap["site-b"] != "https://b.example.com" {
		t.Fatalf("got %q", c.BaseURLMap["site-b"])
	}
}

func TestConfig_LoadBaseURLs_MissingFile(t *testing.T) {
	c := &Config{BaseURLConfigPath: filepath.Join(t.TempDir(), "does-not-exist.json")}
	c.LoadBaseURLs()
	if c.BaseURLMap == nil {
		t.Fatal("expected empty (non-nil) map")
	}
	if len(c.BaseURLMap) != 0 {
		t.Fatalf("expected empty map, got %v", c.BaseURLMap)
	}
}

func TestConfig_LoadBaseURLs_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{BaseURLConfigPath: path}
	c.LoadBaseURLs()
	if c.BaseURLMap == nil || len(c.BaseURLMap) != 0 {
		t.Fatalf("expected empty map on invalid json, got %v", c.BaseURLMap)
	}
}
