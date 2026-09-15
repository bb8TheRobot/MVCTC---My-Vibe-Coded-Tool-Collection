package main

import "testing"

// An excerpt in the shape of a real modules/packages.nix.
const pkgs = `{ pkgs, ... }:
{
  environment.systemPackages = with pkgs; [
    # Hyprland and Quickshell
    quickshell
    kitty

    # Desktop applications
    firefox
    mpv
  ];

  environment.defaultPackages = [ ];
}
`

const svc = `{ ... }:
{
  services.openssh.enable = true;
}
`

func TestAddPackage(t *testing.T) {
	out, err := addPackage(pkgs, "cava")
	if err != nil {
		t.Fatal(err)
	}
	if has, _ := hasPackage(out, "cava"); !has {
		t.Fatal("cava missing after insertion")
	}
	// Has to land in its own block, not in one of the user's sections.
	if !contains(out, "    # via nixpkg\n    cava\n") {
		t.Fatalf("cava not in the nixpkg block:\n%s", out)
	}
	// The list has to stay closed.
	if !contains(out, "  ];") {
		t.Fatal("end of list destroyed")
	}
}

func TestAddPackageSorted(t *testing.T) {
	out, _ := addPackage(pkgs, "cava")
	out, err := addPackage(out, "btop")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out, "    btop\n    cava\n") {
		t.Fatalf("not in alphabetical order:\n%s", out)
	}
}

func TestAddPackageDuplicate(t *testing.T) {
	if _, err := addPackage(pkgs, "firefox"); err == nil {
		t.Fatal("a duplicate firefox has to be rejected")
	}
}

// Removal has to work in the hand-maintained sections too, not only in the
// nixpkg block -- otherwise everything that predates nixpkg gets stuck there.
func TestRemovePackageFromUserSection(t *testing.T) {
	out, err := removePackage(pkgs, "firefox")
	if err != nil {
		t.Fatal(err)
	}
	if has, _ := hasPackage(out, "firefox"); has {
		t.Fatal("firefox still there")
	}
	if has, _ := hasPackage(out, "mpv"); !has {
		t.Fatal("mpv wrongly deleted along with it")
	}
}

func TestRemovePackageCleansUpBlock(t *testing.T) {
	out, _ := addPackage(pkgs, "cava")
	out, err := removePackage(out, "cava")
	if err != nil {
		t.Fatal(err)
	}
	if contains(out, "# via nixpkg") {
		t.Fatalf("empty nixpkg block was left behind:\n%s", out)
	}
	if out != pkgs {
		t.Fatalf("not back to the original state:\n%s", out)
	}
}

func TestRemovePackageMissing(t *testing.T) {
	if _, err := removePackage(pkgs, "steam"); err == nil {
		t.Fatal("a missing package has to produce an error, not silently do nothing")
	}
}

func TestSetService(t *testing.T) {
	out, err := setService(svc, "programs.steam.enable", true)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out, "  programs.steam.enable = true;\n}") {
		t.Fatalf("module line in the wrong place:\n%s", out)
	}
	if !contains(out, "services.openssh.enable = true;") {
		t.Fatal("openssh lost")
	}

	back, err := setService(out, "programs.steam.enable", false)
	if err != nil {
		t.Fatal(err)
	}
	if back != svc {
		t.Fatalf("removal does not yield the original:\n%s", back)
	}
}

func TestSetServiceDuplicate(t *testing.T) {
	if _, err := setService(svc, "services.openssh.enable", true); err == nil {
		t.Fatal("enabling twice has to be rejected")
	}
	if _, err := setService(svc, "programs.steam.enable", false); err == nil {
		t.Fatal("removing a module that is not set has to be rejected")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"cava", "python3Packages.foo", "nerd-fonts.jetbrains-mono", "gtk+"} {
		if !validName.MatchString(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	// Everything that could do damage in a .nix file or in a command.
	for _, bad := range []string{"", "a b", "foo;rm -rf /", "../etc", "$(id)", "a\nb", "]; evil = 1; ["} {
		if validName.MatchString(bad) {
			t.Errorf("%q should have been rejected", bad)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ── imports ─────────────────────────────────────────────────────────────────

const cfgImports = `{ config, pkgs, ... }:
{
  imports = [
    ./hardware-configuration.nix
    ./modules/desktop.nix
  ];

  system.stateVersion = "26.05";
}
`

func TestAddImport(t *testing.T) {
	out, err := addImport(cfgImports, "modules/packages.nix")
	if err != nil {
		t.Fatal(err)
	}
	// Adopt the indentation of the existing entries, do not assume four spaces.
	if !contains(out, "    ./modules/desktop.nix\n    ./modules/packages.nix\n  ];") {
		t.Fatalf("entry sits in the wrong place:\n%s", out)
	}
	// Everything else has to stay untouched.
	if !contains(out, `system.stateVersion = "26.05";`) {
		t.Fatalf("rest of the file damaged:\n%s", out)
	}
}

// The same module listed twice in imports is a build error. The second call
// therefore has to be a no-op, not a second entry.
func TestAddImportAlreadyThere(t *testing.T) {
	out, err := addImport(cfgImports, "modules/desktop.nix")
	if !isNoop(err) {
		t.Fatalf("expected a no-op, got (%q, %v)", out, err)
	}
}

// Single line: better to stop than to rewrite someone else's line.
func TestAddImportSingleLine(t *testing.T) {
	src := "{ ... }:\n{\n  imports = [ ./hardware-configuration.nix ];\n}\n"
	if _, err := addImport(src, "modules/packages.nix"); err == nil {
		t.Fatal("a single-line imports list should have been rejected")
	}
}

// No imports list is created -- where it belongs is not something to guess.
func TestAddImportWithoutList(t *testing.T) {
	src := "{ ... }:\n{\n  system.stateVersion = \"26.05\";\n}\n"
	_, err := addImport(src, "modules/packages.nix")
	if err == nil {
		t.Fatal("a missing imports list should have produced an error")
	}
	if isNoop(err) {
		t.Fatal("a missing list is an error, not a no-op -- otherwise nixpkg reports success while the file is never read")
	}
}

// A commented-out imports line is not a list.
func TestAddImportCommentedOut(t *testing.T) {
	src := "{ ... }:\n{\n  # imports = [ ./old.nix ];\n  system.stateVersion = \"26.05\";\n}\n"
	if _, err := addImport(src, "modules/packages.nix"); err == nil {
		t.Fatal("a commented-out imports list should not have counted")
	}
}
