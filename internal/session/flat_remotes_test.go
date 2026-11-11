package session

// [ui].flat_remotes lists every remote session under one TUI header. It is
// opt-in: unset or false keeps one header per remote.

import (
	"testing"

	"github.com/BurntSushi/toml"
)

func TestFlatRemotes_DefaultsOff(t *testing.T) {
	var ui UISettings
	if ui.GetFlatRemotes() {
		t.Fatalf("GetFlatRemotes() on unset config = true, want false")
	}
}

func TestFlatRemotes_ExplicitTrueEnables(t *testing.T) {
	var cfg UserConfig
	if _, err := toml.Decode("[ui]\nflat_remotes = true\n", &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !cfg.UI.GetFlatRemotes() {
		t.Fatalf("flat_remotes = true reported GetFlatRemotes() = false")
	}
}
