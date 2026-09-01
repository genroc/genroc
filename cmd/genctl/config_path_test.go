package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The config belongs where a CLI's config belongs, and an override must work. os.UserConfigDir
// returns ~/Library/Application Support on macOS and ignores XDG_CONFIG_HOME entirely — a
// surprise that has already cost one config file.
func TestConfigDir_HonoursXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-probe")
	got, err := configDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/tmp/xdg-probe", "genroc"); got != want {
		t.Errorf("configDir() = %q, want %q — XDG_CONFIG_HOME must win", got, want)
	}
}

func TestConfigDir_DefaultsToDotConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	got, err := configDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, filepath.Join(".config", "genroc")) {
		t.Errorf("configDir() = %q, want it under ~/.config/genroc on every platform", got)
	}
	if strings.Contains(got, "Application Support") {
		t.Error("configDir() still points at the macOS GUI-app location")
	}
}

// An upgrade must not silently lose someone's token: the old path is still read.
func TestLoadConfig_FallsBackToTheLegacyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	legacy, err := legacyConfigFilePath()
	if err != nil {
		t.Skip("no legacy path on this platform")
	}
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("server: http://legacy.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig().Server; got != "http://legacy.test" {
		t.Errorf("server = %q; a config at the old path must still be read, or an upgrade loses it", got)
	}
}
