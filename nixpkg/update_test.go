package main

import (
	"os"
	"path/filepath"
	"testing"
)

// withFakeTools puts fake "nix" and "nixos-rebuild" scripts on PATH instead
// of the real ones. update() and rebuild() call exec.Command("nix", ...) and
// exec.Command("nixos-rebuild", ...) directly -- testing through that seam
// avoids reworking run/stream/streamIn themselves.
//
// The fake "nix" mimics the one part of "nix flake update" that the rollback
// logic actually depends on: it rewrites flake.lock on success, exactly like
// the real thing would. Both scripts fail when the matching NIXPKG_TEST_*_FAIL
// env var is set, so each test controls exactly where things break.
func withFakeTools(t *testing.T) {
	t.Helper()
	bin := t.TempDir()

	nix := `#!/bin/sh
if [ "$1" = "flake" ] && [ "$2" = "update" ]; then
	if [ -n "$NIXPKG_TEST_UPDATE_FAIL" ]; then
		echo "fake: nix flake update failed" >&2
		exit 1
	fi
	echo "updated-lock" > "$4/flake.lock"
	exit 0
fi
exit 0
`
	rebuild := `#!/bin/sh
case "$1" in
build)
	if [ -n "$NIXPKG_TEST_BUILD_FAIL" ]; then
		echo "fake: nixos-rebuild build failed" >&2
		exit 1
	fi
	exit 0
	;;
switch)
	if [ -n "$NIXPKG_TEST_SWITCH_FAIL" ]; then
		echo "fake: nixos-rebuild switch failed" >&2
		exit 1
	fi
	exit 0
	;;
esac
exit 0
`
	writeScript(t, filepath.Join(bin, "nix"), nix)
	writeScript(t, filepath.Join(bin, "nixos-rebuild"), rebuild)

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeScript(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// withUpdateState points the package-level state used by update()/rebuild()
// at a scratch directory, the same way write_test.go does for editFile.
func withUpdateState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	oldDir, oldHost, oldDry := configDir, hostName, dry
	oldTouched, oldBackups, oldCreated, oldPreCommitted := touched, backups, created, preCommitted
	configDir, hostName, dry = dir, "testhost", false
	touched, backups, created, preCommitted = map[string]string{}, map[string]string{}, map[string]bool{}, false

	t.Cleanup(func() {
		configDir, hostName, dry = oldDir, oldHost, oldDry
		touched, backups, created, preCommitted = oldTouched, oldBackups, oldCreated, oldPreCommitted
	})
	return dir
}

// Update failure -> flake.lock restored: "nix flake update" itself fails, so
// nothing was ever rebuilt. The pre-update lock file has to come back exactly
// as it was, and no .bak may be left lying around.
func TestUpdateRestoresLockOnUpdateFailure(t *testing.T) {
	withFakeTools(t)
	dir := withUpdateState(t)
	t.Setenv("NIXPKG_TEST_UPDATE_FAIL", "1")

	lockPath := filepath.Join(dir, "flake.lock")
	if err := os.WriteFile(lockPath, []byte("original-lock\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := update(); err == nil {
		t.Fatal("update() should have failed")
	}

	if got := read(t, lockPath); got != "original-lock\n" {
		t.Fatalf("flake.lock not restored:\n%s", got)
	}
	if _, err := os.Stat(lockPath + ".bak"); !os.IsNotExist(err) {
		t.Fatal(".bak still lying around after a restored update failure")
	}
}

// Build failure -> lock restored: the Flake input update itself succeeds (so
// flake.lock is rewritten), but "nixos-rebuild build" then fails. Nothing was
// applied, so the lock file has to go back to its pre-update content just
// like a plain build failure during install/remove would.
func TestUpdateRestoresLockOnBuildFailure(t *testing.T) {
	withFakeTools(t)
	dir := withUpdateState(t)
	t.Setenv("NIXPKG_TEST_BUILD_FAIL", "1")

	lockPath := filepath.Join(dir, "flake.lock")
	if err := os.WriteFile(lockPath, []byte("original-lock\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := update(); err == nil {
		t.Fatal("update() should have failed")
	}

	if got := read(t, lockPath); got != "original-lock\n" {
		t.Fatalf("flake.lock not restored after a build failure:\n%s", got)
	}
	if _, err := os.Stat(lockPath + ".bak"); !os.IsNotExist(err) {
		t.Fatal(".bak still lying around after a restored build failure")
	}
}

// Switch failure -> lock AND backup stay: the build succeeded (so it would
// boot), only activating it failed. Rolling the lock file back here would
// contradict what was just proven buildable, so both the updated flake.lock
// and its .bak have to stay untouched for a human to sort out.
func TestUpdateKeepsLockAndBackupOnSwitchFailure(t *testing.T) {
	withFakeTools(t)
	dir := withUpdateState(t)
	t.Setenv("NIXPKG_TEST_SWITCH_FAIL", "1")

	lockPath := filepath.Join(dir, "flake.lock")
	if err := os.WriteFile(lockPath, []byte("original-lock\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := update(); err == nil {
		t.Fatal("update() should have failed")
	}

	if got := read(t, lockPath); got != "updated-lock\n" {
		t.Fatalf("flake.lock should stay updated after a switch failure, got:\n%s", got)
	}
	bak, ok := backups[lockPath]
	if !ok {
		t.Fatal("the backup entry should still be tracked after a switch failure")
	}
	if got := read(t, bak); got != "original-lock\n" {
		t.Fatalf(".bak should still hold the pre-update lock file:\n%s", got)
	}
}

// Dry run touches nothing: without root, update() must not touch flake.lock
// at all -- not read it for a backup, not write to it, nothing. A config
// directory without a flake.lock at all is exactly what would catch a dry run
// that tried anyway.
func TestUpdateDryRunTouchesNothing(t *testing.T) {
	withFakeTools(t)
	dir := withUpdateState(t)
	dry = true

	if err := update(); err != nil {
		t.Fatalf("a dry run must not fail: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "flake.lock")); !os.IsNotExist(err) {
		t.Fatal("dry run created a flake.lock that was never there")
	}
	if len(backups) != 0 || len(touched) != 0 || len(created) != 0 {
		t.Fatalf("dry run left state behind: backups=%v touched=%v created=%v", backups, touched, created)
	}
}

// A full success has to rewrite flake.lock, apply it, and leave no backup
// behind -- the counterpart to the three failure paths above.
func TestUpdateSucceeds(t *testing.T) {
	withFakeTools(t)
	dir := withUpdateState(t)

	lockPath := filepath.Join(dir, "flake.lock")
	if err := os.WriteFile(lockPath, []byte("original-lock\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := update(); err != nil {
		t.Fatalf("update() should have succeeded: %v", err)
	}

	if got := read(t, lockPath); got != "updated-lock\n" {
		t.Fatalf("flake.lock was not updated:\n%s", got)
	}
	if _, err := os.Stat(lockPath + ".bak"); !os.IsNotExist(err) {
		t.Fatal(".bak should be cleaned up after a full success")
	}
}
