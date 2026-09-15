// nixpkg manages packages in a NixOS Flake configuration.
//
//	nixpkg -s cava              search (no sudo needed)
//	sudo nixpkg -u              update the Flake + rebuild
//	sudo nixpkg -i cava steam   install + rebuild
//	sudo nixpkg -r cava         remove + rebuild + collect garbage
//
// Without root everything is a dry run: it only shows what would happen.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// Target of all changes. Fallback values only: normally both come from the
// one-time setup, and "-config"/"-host" beat even those. The host name is read
// from the Flake itself on the first run.
var (
	configDir = "/etc/nixos"
	hostName  = "nixos"
)

// Kept instead of "-d": a broken rebuild needs an older generation in the boot
// menu. Matches nix.gc in configuration.nix.
var gcArgs = []string{"--delete-older-than", "30d"}

// Where a new module line goes when the option is not set anywhere yet. The
// first file wins that exists AND is imported by configuration.nix -- writing
// into a file nobody imports would have no effect and nobody would notice.
var serviceTargets = []string{"modules/services.nix", "modules/desktop.nix", "configuration.nix"}

// serviceModules are packages whose NixOS module does more than drop a binary
// in place -- drivers, udev rules, firewall, wrappers. As a plain line in
// systemPackages they would be broken or half useless. Anything not listed
// here becomes a normal package, even when a module of the same name happens
// to exist (programs.firefox, for instance, only sets up enterprise policies,
// which is not what anyone wants here).
var serviceModules = map[string]string{
	"steam":        "programs.steam",
	"hyprland":     "programs.hyprland",
	"gamemode":     "programs.gamemode",
	"gamescope":    "programs.gamescope",
	"wireshark":    "programs.wireshark",
	"virt-manager": "programs.virt-manager",
	"thunar":       "programs.thunar",
	"dconf":        "programs.dconf",
	"nix-ld":       "programs.nix-ld",
	"adb":          "programs.adb",
}

var (
	dry     bool // no root -> nothing is written
	verbose bool
)

func main() {
	var (
		doSearch  = flag.Bool("s", false, "search nixpkgs for packages")
		doInstall = flag.Bool("i", false, "install packages")
		doRemove  = flag.Bool("r", false, "remove packages and collect garbage")
		doUpdate  = flag.Bool("u", false, "update the Flake and rebuild the system")
		asService = flag.Bool("service", false, "treat as a NixOS module (instead of a package)")
		asPackage = flag.Bool("package", false, "treat as a plain package (instead of a module)")
		showAll   = flag.Bool("a", false, "with -s, also show libraries and language packages")
	)
	flag.BoolVar(&verbose, "v", false, "print the commands being run")
	flag.StringVar(&configDir, "config", configDir, "path to the NixOS configuration")
	flag.StringVar(&hostName, "host", hostName, "host name in the Flake")
	flag.Usage = usage
	flag.Parse()

	names := flag.Args()
	if *asService && *asPackage {
		die("-service and -package are mutually exclusive")
	}

	n := 0
	for _, b := range []bool{*doSearch, *doInstall, *doRemove, *doUpdate} {
		if b {
			n++
		}
	}
	if n != 1 {
		usage()
		os.Exit(2)
	}
	if *doUpdate && (len(names) != 0 || *asService || *asPackage || *showAll) {
		die("-u takes no package names and no -service/-package/-a flags")
	}
	if !*doUpdate && len(names) == 0 {
		die("no packages given")
	}

	// Search terms are regexes ("^cava$") and never end up in a file -- they
	// go to nix search as separate arguments, so no shell is involved.
	if *doSearch {
		searchPackages(names, *showAll)
		return
	}

	for _, name := range names {
		if !validName.MatchString(name) {
			die("invalid package name: %q", name)
		}
	}

	// Only after the search branch: -s needs neither path nor host name, and
	// someone who only searches should not see a questionnaire.
	applySettings()

	// A relative value ("-config .") would be ambiguous both for filepath.Join
	// and for the Flake reference.
	abs, err := filepath.Abs(configDir)
	if err != nil {
		die("config path %q: %v", configDir, err)
	}
	configDir = abs

	dry = os.Geteuid() != 0
	if dry {
		fmt.Println("== Dry run (no root) -- nothing will be changed ==")
	} else {
		// Two concurrent runs would both read the old state and the second
		// would overwrite the first.
		lockConfig()
	}

	if *doUpdate {
		if err := update(); err != nil {
			die("%v", err)
		}
		return
	}

	changed := 0
	for _, name := range names {
		var err error
		if *doInstall {
			err = install(name, *asService, *asPackage)
		} else {
			err = remove(name, *asService, *asPackage)
		}
		switch {
		case isNoop(err):
			// No reason to throw away the whole run -- the rest of the package
			// list should still go through.
			fmt.Printf("\n%s: %v -- skipped\n", name, err)
		case err != nil:
			abort("%s: %v", name, err)
		default:
			changed++
		}
	}

	if changed == 0 {
		fmt.Println("\nNothing changed -- no rebuild needed.")
		return
	}
	if dry {
		fmt.Println("\nRun it with sudo to actually do this.")
		return
	}

	// Mandatory before the rebuild: a Flake only sees files that Git knows.
	gitAdd()
	if err := rebuild(); err != nil {
		die("%v", err)
	}
	if *doRemove {
		collectGarbage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `nixpkg -- manage packages in a NixOS configuration

  nixpkg -s <term>...        search
  sudo nixpkg -i <pkg>...    install + rebuild
  sudo nixpkg -r <pkg>...    remove + rebuild + collect garbage
  sudo nixpkg -u             update all Flake inputs + rebuild

Without sudo, -i/-r/-u run as a dry run and show the planned changes.

  -a         with -s, also show libraries and language packages
  -service   force the module line (programs.<name>.enable)
  -package   force the package line in systemPackages
  -v         print the commands being run
  -config    use a different configuration than the configured one
  -host      use a different host name than the configured one

On the first run, nixpkg asks for the configuration path, the Flake host name
and the target files for packages and module switches, and stores them in
%s.
nixpkg requires a Flake-based configuration.
`, settingsPathOrName())
}

// ── Actions ─────────────────────────────────────────────────────────────────

func install(name string, forceService, forcePackage bool) error {
	if !forceService {
		if _, err := run("nix", "eval", "--raw", "nixpkgs#"+name+".name"); err != nil {
			return fmt.Errorf("does not exist in nixpkgs (nix search nixpkgs %s)", name)
		}
	}

	mod, err := moduleFor(name, forceService, forcePackage)
	if err != nil {
		return err
	}
	if mod != "" {
		rel, err := serviceFile(mod + ".enable")
		if err != nil {
			return err
		}
		return editTarget(rel, "enable "+mod,
			func(s string) (string, error) { return setService(s, mod+".enable", true) })
	}

	// systemPackages may appear in several modules. Look everywhere first,
	// otherwise a package listed in desktop.nix lands in packages.nix a second
	// time.
	if rel, ok := findModule(hasPackageIn(name)); ok {
		return noopf("%s is already in %s", name, rel)
	}
	rel, err := packageFile()
	if err != nil {
		return err
	}
	return editTarget(rel, "install "+name,
		func(s string) (string, error) { return addPackage(s, name) })
}

func remove(name string, forceService, forcePackage bool) error {
	mod, err := moduleFor(name, forceService, forcePackage)
	if err != nil {
		return err
	}
	if mod != "" {
		rel, err := serviceFile(mod + ".enable")
		if err != nil {
			return err
		}
		return editTarget(rel, "disable "+mod,
			func(s string) (string, error) { return setService(s, mod+".enable", false) })
	}

	rel, ok := findModule(hasPackageIn(name))
	if !ok {
		return noopf("%s is not in any package list", name)
	}
	return editTarget(rel, "remove "+name,
		func(s string) (string, error) { return removePackage(s, name) })
}

// moduleFor decides package vs. module and returns the option path
// ("" = plain package).
func moduleFor(name string, forceService, forcePackage bool) (string, error) {
	if forcePackage {
		return "", nil
	}
	mod, known := serviceModules[name]
	if !known {
		if !forceService {
			return "", nil
		}
		mod = "programs." + name
	}
	if !optionExists(mod + ".enable") {
		if forceService {
			return "", fmt.Errorf("%s.enable does not exist in this NixOS version", mod)
		}
		// A module from the list is gone in this nixpkgs version -> package.
		return "", nil
	}
	return mod, nil
}

func optionExists(path string) bool {
	attr := fmt.Sprintf("%s#nixosConfigurations.%s.options.%s", configDir, hostName, path)
	_, err := run("nix", "eval", attr)
	return err == nil
}

// ── Finding modules ─────────────────────────────────────────────────────────

// serviceFile returns the module in which the switch has to be changed:
// preferably the one that already holds the option. A second definition in
// another file breaks the rebuild ("attribute already defined").
func serviceFile(optPath string) (string, error) {
	if rel, ok := findModule(func(src string) (bool, error) { return definesOption(src, optPath), nil }); ok {
		return rel, nil
	}
	// The configured file wins -- but only here: if the option already lives
	// somewhere, it has to stay there or it ends up defined twice.
	if cfg.ServicesFile != "" {
		return cfg.ServicesFile, nil
	}
	for _, rel := range serviceTargets {
		if !isImported(rel) {
			continue
		}
		if _, ok := readModule(rel); ok {
			return rel, nil
		}
	}
	return "", fmt.Errorf("no imported module found that %s could be written to", optPath)
}

// packageFile returns the module a new package is written to.
func packageFile() (string, error) {
	// Whoever configured a file gets it -- even if it does not exist yet.
	// editTarget creates it in that case.
	if cfg.PackagesFile != "" {
		return cfg.PackagesFile, nil
	}
	const preferred = "modules/packages.nix"
	if isImported(preferred) {
		if _, ok := readModule(preferred); ok {
			return preferred, nil
		}
	}
	rel, ok := findModule(func(src string) (bool, error) {
		lists, err := pkgLists(strings.Split(src, "\n"))
		return len(lists) > 0, err
	})
	if !ok {
		return "", fmt.Errorf("no imported module with environment.systemPackages found")
	}
	return rel, nil
}

// findModule returns the first imported module matching pred.
// A module pred cannot read is skipped -- but loudly, with a reason. Skipping
// silently would mean "not in any package list" when the truthful answer is
// unknown. Hard-aborting would be wrong too, as one unusually formatted
// foreign file would then paralyse the whole tool.
func findModule(pred func(src string) (bool, error)) (string, bool) {
	for _, rel := range importedFiles() {
		src, ok := readModule(rel)
		if !ok {
			continue
		}
		hit, err := pred(src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nixpkg: skipped %s: %v\n", rel, err)
			continue
		}
		if hit {
			return rel, true
		}
	}
	return "", false
}

// hasPackageIn is the predicate for findModule: is name in this file's package
// list?
func hasPackageIn(name string) func(string) (bool, error) {
	return func(src string) (bool, error) {
		has, _, err := findPackage(src, name)
		return has, err
	}
}

// readModule reads a module -- this run's state if it has been touched
// already, otherwise from disk.
func readModule(rel string) (string, bool) {
	path := filepath.Join(configDir, rel)
	if s, ok := touched[path]; ok {
		return s, true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

var (
	importsCache []string
	importPath   = regexp.MustCompile(`\./([A-Za-z0-9._/+-]+\.nix)`)
)

// importedFiles returns configuration.nix and everything it imports. Only
// those files are read and written: a line in a module nobody imports has no
// effect, and that goes unnoticed -- the diff looks right and the rebuild runs
// through cleanly.
func importedFiles() []string {
	if importsCache != nil {
		return importsCache
	}
	out := []string{"configuration.nix"}
	b, err := os.ReadFile(filepath.Join(configDir, "configuration.nix"))
	if err != nil {
		abort("cannot read configuration.nix: %v", err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		for _, m := range importPath.FindAllStringSubmatch(stripComment(l), -1) {
			// Generated by nixos-generate-config, nixpkg never writes there.
			if m[1] == "hardware-configuration.nix" {
				continue
			}
			out = append(out, m[1])
		}
	}
	importsCache = out
	return out
}

func isImported(rel string) bool {
	for _, f := range importedFiles() {
		if f == rel {
			return true
		}
	}
	return false
}

// ── Writing files ───────────────────────────────────────────────────────────

// touched holds the current state of every file this run has modified.
// Without it, "nixpkg -i a b" would read the second step's state from disk
// again -- the dry run would then show nonsense, and the second package's .bak
// would save the intermediate state instead of the original.
var touched = map[string]string{}

// backups are the .bak files THIS run created (path -> .bak). A fixed file
// list would be wrong: after an aborted run an old .bak may be lying around,
// and restore() would use it to steamroll changes that have nothing to do with
// this run.
var backups = map[string]string{}

// preCommitted makes sure the safety commit only runs once, even when a single
// run touches several modules.
var preCommitted bool

// created are the files THIS run newly created. They have no backup -- they
// are taken back by deleting them.
var created = map[string]bool{}

// isTarget reports whether rel is a configured target file. Only those may be
// created by nixpkg: creating any missing file would silently turn a typo in a
// path into a new module file.
func isTarget(rel string) bool {
	return rel != "" && (rel == cfg.PackagesFile || rel == cfg.ServicesFile)
}

// editTarget is editFile for the target files of packages and switches: it
// creates a configured but still missing file and adds it to the imports
// before editing it.
//
// The order is deliberate: first the file, then the import. The other way
// round, configuration.nix would briefly point at a file that does not exist
// -- and aborting exactly in between would leave a system that no longer
// builds. The reverse intermediate state is harmless: a file nobody reads yet.
func editTarget(rel, what string, edit func(string) (string, error)) error {
	if err := ensureFile(rel); err != nil {
		return err
	}
	if err := ensureImported(rel); err != nil {
		return err
	}
	return editFile(rel, what, edit)
}

// ensureFile creates a configured target file when it is missing.
func ensureFile(rel string) error {
	if _, seen := touched[filepath.Join(configDir, rel)]; seen {
		return nil
	}
	if _, err := os.Stat(filepath.Join(configDir, rel)); err == nil {
		return nil
	}
	if !isTarget(rel) {
		return nil // not configured -> editFile reports it as missing
	}
	// "modules/packages.nix" means modules/ has to exist. Without this,
	// writeFile is the first thing to fail -- mid-run, after the safety commit.
	if dir := filepath.Dir(filepath.Join(configDir, rel)); !dry {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return editFile(rel, "create "+rel, func(string) (string, error) {
		return skeleton(rel == cfg.PackagesFile), nil
	})
}

// ensureImported makes sure rel appears in the imports list. Without that
// entry, nixpkg writes into a file NixOS never reads.
func ensureImported(rel string) error {
	if rel == "configuration.nix" || isImported(rel) {
		return nil
	}
	err := editFile("configuration.nix", "add "+rel+" to imports",
		func(src string) (string, error) { return addImport(src, rel) })
	if err != nil && !isNoop(err) {
		return err
	}
	importsCache = nil // the list is a different one now
	return nil
}

// skeleton is the content of a freshly created module file.
func skeleton(withPackages bool) string {
	head := "# created by nixpkg\n{ ... }:\n{\n"
	if withPackages {
		head = "# created by nixpkg\n{ pkgs, ... }:\n{\n  environment.systemPackages = with pkgs; [\n  ];\n"
	}
	return head + "}\n"
}

// editFile applies edit to a file of the configuration: a backup on first
// touch, then the write. In a dry run it only shows the diff.
func editFile(rel, what string, edit func(string) (string, error)) error {
	path := filepath.Join(configDir, rel)

	old, seen := touched[path]
	if !seen {
		b, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err) && isTarget(rel):
			// A configured target file that does not exist yet: it will be
			// created. There is no backup for that -- it is taken back by
			// restore() deleting it again.
			created[path] = true
		case os.IsNotExist(err):
			return fmt.Errorf("%s is missing -- create it and import it in configuration.nix", rel)
		case err != nil:
			return err
		default:
			old = string(b)
		}
	}

	next, err := edit(old)
	if err != nil {
		return err
	}

	fmt.Printf("\n%s -> %s\n", what, rel)
	printDiff(old, next)

	if dry {
		touched[path] = next
		return nil
	}
	// Exactly once per run, and only once something is really written: the
	// state before is worth a commit, every intermediate state is not -- and a
	// run that changes nothing in the end should not commit anything either.
	if !preCommitted {
		gitCommit("before nixpkg: " + what)
		preCommitted = true
	}
	// A file this run creates from scratch needs no backup: there is no old
	// state. An empty .bak would even be harmful -- restore() would use it to
	// write an empty .nix and break the rebuild.
	if !seen && !created[path] {
		if err := writeFile(path+".bak", old); err != nil {
			return fmt.Errorf("backup failed: %w", err)
		}
		backups[path] = path + ".bak"
	}
	if err := writeFile(path, next); err != nil {
		return err
	}
	touched[path] = next
	return nil
}

// writeFile writes atomically: first a file next to it, then a rename. Writing
// straight into the target file means an abort halfway through leaves half a
// packages.nix behind -- and with it a system that no longer builds.
func writeFile(path, content string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".nixpkg-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after the rename, otherwise it cleans up

	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	// Without Sync, a power cut shortly after the rename can leave an empty
	// file -- exactly the case the rename is supposed to protect against.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0644); err != nil { // CreateTemp makes it 0600
		return err
	}
	return os.Rename(tmp, path)
}

// restore puts this run's backups back and reports whether that worked
// completely. Used when something goes wrong after writing.
func restore() bool {
	ok := true
	// Newly created files first, then the modified ones back: the imports line
	// pointing at such a file sits inside one of the backups.
	for path := range created {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "nixpkg: %s NOT removed (%v) -- please delete it by hand\n", path, err)
			ok = false
			continue
		}
		delete(created, path)
		fmt.Printf("removed again: %s\n", path)
	}
	for path, bak := range backups {
		data, err := os.ReadFile(bak)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nixpkg: cannot read backup %s (%v) -- please check %s by hand\n", bak, err, path)
			ok = false
			continue
		}
		if err := writeFile(path, string(data)); err != nil {
			fmt.Fprintf(os.Stderr, "nixpkg: %s NOT restored (%v) -- the original is in %s\n", path, err, bak)
			ok = false
			continue
		}
		os.Remove(bak)
		delete(backups, path)
		fmt.Printf("restored: %s\n", path)
	}
	return ok
}

func cleanBackups() {
	for path, bak := range backups {
		os.Remove(bak)
		delete(backups, path)
	}
}

// bakPaths names the remaining backups, so the message in the failure case can
// say where the old state is.
func bakPaths() string {
	var out []string
	for _, bak := range backups {
		out = append(out, bak)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// lockConfig prevents two nixpkg runs from reading and writing the same file
// at the same time. The lock hangs off the directory itself -- nothing is
// created, and the kernel releases it on process exit by itself.
var lockFD *os.File

func lockConfig() {
	f, err := os.Open(configDir)
	if err != nil {
		die("cannot read %s: %v", configDir, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		die("another nixpkg is already running on %s (%v)", configDir, err)
	}
	lockFD = f // hold on to it, or the GC closes the file and drops the lock
}

// printDiff shows only the changed lines. That is enough, because exactly one
// line is added or removed at a time.
func printDiff(before, after string) {
	b := strings.Split(before, "\n")
	a := strings.Split(after, "\n")
	inB := map[string]int{}
	for _, l := range b {
		inB[l]++
	}
	inA := map[string]int{}
	for _, l := range a {
		inA[l]++
	}
	for _, l := range a {
		if strings.TrimSpace(l) != "" && inA[l] > inB[l] {
			fmt.Printf("  \033[32m+ %s\033[0m\n", strings.TrimSpace(l))
			inB[l]++
		}
	}
	for _, l := range b {
		if strings.TrimSpace(l) != "" && inB[l] > inA[l] {
			fmt.Printf("  \033[31m- %s\033[0m\n", strings.TrimSpace(l))
			inA[l]++
		}
	}
}

// ── System ──────────────────────────────────────────────────────────────────

func update() error {
	if dry {
		fmt.Printf("nix flake update --flake %q\n", configDir)
		fmt.Printf("nixos-rebuild build --flake %q\n", configDir+"#"+hostName)
		fmt.Printf("nixos-rebuild switch --flake %q\n", configDir+"#"+hostName)
		fmt.Println("\nRun it with sudo to actually do this.")
		return nil
	}

	path := filepath.Join(configDir, "flake.lock")
	old, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read flake.lock (an update needs an existing lock file): %w", err)
	}
	gitCommit("before nixpkg: update")
	if err := writeFile(path+".bak", string(old)); err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}
	backups[path] = path + ".bak"
	fmt.Println("\nUpdating Flake inputs...")
	if err := stream("nix", "flake", "update", "--flake", configDir); err != nil {
		if restore() {
			return fmt.Errorf("update failed -- old lock file restored: %w", err)
		}
		return fmt.Errorf("update and rollback both failed -- backup: %s: %w", bakPaths(), err)
	}
	gitAdd()
	return rebuild()
}

func rebuild() error {
	flake := configDir + "#" + hostName

	// "nixos-rebuild build" drops a result symlink into the current directory.
	// That symlink is a GC root: it pins the entire system closure (~18 GiB),
	// even long after the generation has rotated out. So nixpkg builds in a
	// temporary directory of its own, which disappears along with the symlink
	// afterwards. The Flake reference is absolute, so the working directory
	// does not matter for the build.
	tmp, err := os.MkdirTemp("", "nixpkg-build-")
	if err != nil {
		return fmt.Errorf("temp directory for the build: %v", err)
	}
	defer os.RemoveAll(tmp)

	fmt.Println("\nBuilding the configuration...")
	if err := streamIn(tmp, "nixos-rebuild", "build", "--flake", flake); err != nil {
		if restore() {
			return fmt.Errorf("build failed -- the old state is back, nothing was applied")
		}
		return fmt.Errorf("build failed AND the rollback went wrong -- please check the configuration by hand (%s)", bakPaths())
	}

	fmt.Println("\nActivating...")
	if err := stream("nixos-rebuild", "switch", "--flake", flake); err != nil {
		return fmt.Errorf("switch failed (files stay, the old generation keeps running)\n"+
			"       the old state is in: %s", bakPaths())
	}

	cleanBackups()
	gitCommit("nixpkg: applied")
	fmt.Println("\n\033[32mDone.\033[0m")
	return nil
}

func collectGarbage() {
	fmt.Printf("\nCollecting garbage (%s)...\n", strings.Join(gcArgs, " "))
	// It reports "N store paths deleted, X MiB freed" itself at the end.
	if err := stream("nix-collect-garbage", gcArgs...); err != nil {
		fmt.Fprintln(os.Stderr, "Collecting garbage failed -- the package is gone regardless.")
	}
}

func isRepo() bool {
	_, err := run("git", "-C", configDir, "rev-parse", "--git-dir")
	return err == nil
}

func gitCommit(msg string) {
	if !isRepo() {
		return // not a repo -> the .bak file has to be enough
	}
	gitAdd()
	if out, err := run("git", "-C", configDir, "commit", "-m", msg); err != nil && verbose {
		fmt.Fprintf(os.Stderr, "nixpkg: git commit skipped (%v)\n%s", err, out)
	}
}

// gitAdd has to run before every rebuild: a Flake only sees files that Git
// knows -- a new, untracked file aborts the rebuild. The .bak files are
// working files and do not belong in the history.
func gitAdd() {
	if !isRepo() {
		return
	}
	if out, err := run("git", "-C", configDir, "add", "-A", ":!*.bak"); err != nil {
		fmt.Fprintf(os.Stderr, "nixpkg: git add failed (%v)\n%s", err, out)
	}
}

// run starts a program quietly and collects its output -- for the short
// queries whose output would only get in the way.
func run(name string, args ...string) (string, error) {
	trace(name, args)
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// stream starts a program and passes its output straight through -- for
// everything that takes a while (search, rebuild, garbage collection). The
// progress, and on failure the nix message, reach the terminal unchanged.
func stream(name string, args ...string) error {
	return streamIn("", name, args...)
}

// streamIn is stream with a fixed working directory -- needed for
// "nixos-rebuild build", which puts its result symlink there.
func streamIn(dir, name string, args ...string) error {
	trace(name, args)
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// Both paths start the program directly, never through a shell -- a package
// name can therefore never be interpreted as a command.
func trace(name string, args []string) {
	if verbose {
		fmt.Fprintf(os.Stderr, "+ %s %s\n", name, strings.Join(args, " "))
	}
}

// abort is the exit taken after something has already been written: roll
// everything back first, then leave. A bare die() would leave the
// configuration half-changed, with a .bak next to it that nobody applies.
func abort(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "nixpkg: "+format+"\n", a...)
	restore()
	os.Exit(1)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "nixpkg: "+format+"\n", a...)
	os.Exit(1)
}
