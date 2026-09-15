package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Every test here points XDG_CONFIG_HOME at a t.TempDir() -- the user's real
// ~/.config is never touched.
func withConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

func TestSettingsRoundtrip(t *testing.T) {
	dir := withConfigHome(t)

	want := &settings{ConfigDir: "/srv/nix", HostName: "box"}
	if err := saveSettings(want); err != nil {
		t.Fatalf("saveSettings: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nixpkg", "config.json")); err != nil {
		t.Fatalf("config.json was not created: %v", err)
	}

	got, err := readSettings()
	if err != nil {
		t.Fatalf("readSettings: %v", err)
	}
	if got == nil || *got != *want {
		t.Fatalf("read %+v, want %+v", got, want)
	}
}

// Before setup there is no file. That is not an error but exactly the signal
// that triggers the questionnaire -- if an error came back here, setup would
// never start.
func TestReadSettingsMissingIsNotAnError(t *testing.T) {
	withConfigHome(t)

	s, err := readSettings()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s != nil {
		t.Fatalf("expected nil, got %+v", s)
	}
}

// A half-written or hand-mangled file has to come back as an error, so that
// applySettings asks again instead of carrying on with zero values --
// configDir = "" would otherwise point at "/".
func TestReadSettingsBrokenJSON(t *testing.T) {
	dir := withConfigHome(t)

	path := filepath.Join(dir, "nixpkg", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"configDir": "/srv/nix"`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := readSettings(); err == nil {
		t.Fatal("truncated JSON should have produced an error")
	}
}

// SUDO_USER may only count when the run really is root. Otherwise a stray
// SUDO_USER could redirect an ordinary run into someone else's home.
func TestSudoUserWithoutRootIsIgnored(t *testing.T) {
	dir := withConfigHome(t)
	t.Setenv("SUDO_USER", "root")

	if os.Geteuid() == 0 {
		t.Skip("this test assumes a run without root")
	}
	got, err := userConfigHome()
	if err != nil {
		t.Fatalf("userConfigHome: %v", err)
	}
	if got != dir {
		t.Fatalf("got %q, want %q -- SUDO_USER was honoured without root", got, dir)
	}
}

// detectConfigDir may only suggest a path that holds a flake.nix, and
// otherwise has to fall back to the first candidate.
func TestDetectConfigDir(t *testing.T) {
	dir := t.TempDir()
	orig := configCandidates
	t.Cleanup(func() { configCandidates = orig })

	configCandidates = []string{filepath.Join(dir, "empty"), filepath.Join(dir, "real")}
	if err := os.MkdirAll(configCandidates[1], 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configCandidates[1], "flake.nix"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := detectConfigDir(); got != configCandidates[1] {
		t.Fatalf("got %q, want %q", got, configCandidates[1])
	}

	configCandidates = []string{filepath.Join(dir, "empty")}
	if got := detectConfigDir(); got != configCandidates[0] {
		t.Fatalf("without a hit, want %q, got %q", configCandidates[0], got)
	}
}

// Paths written into config.json by hand are not validated. One that points
// outside the configuration has to be discarded -- otherwise nixpkg creates
// files somewhere in the file system.
func TestTargetOutsideConfigIsDiscarded(t *testing.T) {
	for _, rel := range []string{"../elsewhere.nix", "/etc/passwd", "modules/../../away.nix"} {
		s := settings{PackagesFile: rel, ServicesFile: rel}
		s.dropEscapingTargets()
		if s.PackagesFile != "" || s.ServicesFile != "" {
			t.Errorf("%q got through: %+v", rel, s)
		}
	}
	// Counter-check: the real values have to pass unchanged.
	want := settings{PackagesFile: "configuration.nix", ServicesFile: "modules/packages.nix"}
	got := want
	got.dropEscapingTargets()
	if got != want {
		t.Errorf("valid paths discarded: %+v", got)
	}
}
