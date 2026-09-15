package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The write path otherwise only runs as root. Here it is driven against a temp
// directory: the backup has to be correct, and after a failed build restore()
// has to reproduce the starting state exactly -- otherwise an aborted run
// leaves the configuration half-changed.
func TestWriteAndRollback(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "modules"), 0755); err != nil {
		t.Fatal(err)
	}
	pkgPath := filepath.Join(dir, "modules/packages.nix")
	if err := os.WriteFile(pkgPath, []byte(pkgs), 0644); err != nil {
		t.Fatal(err)
	}

	oldDir, oldDry, oldTouched := configDir, dry, touched
	configDir, dry, touched = dir, false, map[string]string{}
	defer func() { configDir, dry, touched = oldDir, oldDry, oldTouched }()

	// Two packages in one run -- that is exactly where the backup used to be
	// wrong.
	for _, name := range []string{"ripgrep", "htop"} {
		n := name
		err := editFile("modules/packages.nix", "install "+n,
			func(s string) (string, error) { return addPackage(s, n) })
		if err != nil {
			t.Fatal(err)
		}
	}

	got := read(t, pkgPath)
	for _, name := range []string{"ripgrep", "htop"} {
		if has, _ := hasPackage(got, name); !has {
			t.Fatalf("%s was not written", name)
		}
	}

	// The backup has to be the original, not the intermediate state after the
	// first package.
	if bak := read(t, pkgPath+".bak"); bak != pkgs {
		t.Fatalf("backup is not the original:\n%s", bak)
	}

	restore()

	if after := read(t, pkgPath); after != pkgs {
		t.Fatalf("restore() did not reproduce the starting state:\n%s", after)
	}
	if _, err := os.Stat(pkgPath + ".bak"); !os.IsNotExist(err) {
		t.Fatal(".bak is still lying around after restore()")
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A configured target file may live in a directory that does not exist yet
// ("modules/packages.nix" in a configuration without modules/). writeFile does
// not create directories -- without the MkdirAll in ensureFile the run would
// only fail at write time, i.e. after the safety commit.
func TestWriteFileWithoutDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, "modules", "packages.nix"), "{}"); err == nil {
		t.Fatal("writeFile creates directories -- then the MkdirAll in ensureFile is redundant")
	}
}

// The whole path for a configured file that does not exist yet: create it, add
// it to the imports, write a package into it -- and after a failed build none
// of that may remain. The file has no backup, so it has to be deleted;
// configuration.nix has one and has to be written back. If the imports line
// stayed while the file was gone, the system would no longer build.
func TestCreateTargetAndRollback(t *testing.T) {
	dir := t.TempDir()
	const cfgSrc = `{ config, pkgs, ... }:
{
  imports = [
    ./hardware-configuration.nix
  ];
}
`
	if err := os.WriteFile(filepath.Join(dir, "configuration.nix"), []byte(cfgSrc), 0644); err != nil {
		t.Fatal(err)
	}

	oldDir, oldDry, oldTouched, oldCfg, oldCache := configDir, dry, touched, cfg, importsCache
	configDir, dry, touched = dir, false, map[string]string{}
	cfg = settings{PackagesFile: "modules/packages.nix"}
	importsCache = nil
	defer func() {
		configDir, dry, touched, cfg, importsCache = oldDir, oldDry, oldTouched, oldCfg, oldCache
		created = map[string]bool{}
		backups = map[string]string{}
	}()

	rel, err := packageFile()
	if err != nil {
		t.Fatal(err)
	}
	if rel != "modules/packages.nix" {
		t.Fatalf("configured file ignored, got %q", rel)
	}
	if err := editTarget(rel, "install cava",
		func(s string) (string, error) { return addPackage(s, "cava") }); err != nil {
		t.Fatal(err)
	}

	pkgPath := filepath.Join(dir, rel)
	if has, _ := hasPackage(read(t, pkgPath), "cava"); !has {
		t.Fatal("cava is not in the newly created file")
	}
	if !contains(read(t, filepath.Join(dir, "configuration.nix")), "./modules/packages.nix") {
		t.Fatal("the new file was not added to the imports")
	}

	restore()

	if _, err := os.Stat(pkgPath); !os.IsNotExist(err) {
		t.Fatal("the created file is still there after restore()")
	}
	if got := read(t, filepath.Join(dir, "configuration.nix")); got != cfgSrc {
		t.Fatalf("configuration.nix not restored -- imports points at a file that no longer exists:\n%s", got)
	}
}

// The intermediate state that hurts most: the file is created, but the imports
// entry fails (here on a single-line imports list, which addImport rejects on
// purpose). Afterwards nothing may be left behind in the user's configuration
// -- neither an orphaned module file nor a half-edited configuration.nix.
func TestCreateTargetImportFails(t *testing.T) {
	dir := t.TempDir()
	const cfgSrc = `{ config, pkgs, ... }:
{
  imports = [ ./hardware-configuration.nix ];
}
`
	if err := os.WriteFile(filepath.Join(dir, "configuration.nix"), []byte(cfgSrc), 0644); err != nil {
		t.Fatal(err)
	}

	oldDir, oldDry, oldTouched, oldCfg, oldCache := configDir, dry, touched, cfg, importsCache
	configDir, dry, touched = dir, false, map[string]string{}
	cfg = settings{PackagesFile: "modules/packages.nix"}
	importsCache = nil
	defer func() {
		configDir, dry, touched, cfg, importsCache = oldDir, oldDry, oldTouched, oldCfg, oldCache
		created = map[string]bool{}
		backups = map[string]string{}
	}()

	err := editTarget("modules/packages.nix", "install cava",
		func(s string) (string, error) { return addPackage(s, "cava") })
	if err == nil {
		t.Fatal("a single-line imports list should have aborted the run")
	}
	// Without this check the test would also pass if the run had aborted
	// before creating anything -- then it would be testing nothing at all.
	if !created[filepath.Join(dir, "modules/packages.nix")] {
		t.Fatal("the file was never created -- this test is checking the wrong abort")
	}

	restore()

	if _, err := os.Stat(filepath.Join(dir, "modules/packages.nix")); !os.IsNotExist(err) {
		t.Fatal("an orphaned module file is left in the user's configuration")
	}
	if got := read(t, filepath.Join(dir, "configuration.nix")); got != cfgSrc {
		t.Fatalf("configuration.nix is not unchanged:\n%s", got)
	}
}
