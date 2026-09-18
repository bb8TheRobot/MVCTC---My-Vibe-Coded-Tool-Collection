package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// settings holds what nixpkg cannot work out about someone else's
// installation, plus the target files the user asked for.
//
// The target files are an instruction, not a description: left empty, nixpkg
// looks for them itself as before (importedFiles() reads the imports list,
// packageFile() looks for the file holding the systemPackages list -- a single
// configuration.nix and a split configuration both work without help). They
// are filled in because somebody explicitly wanted a particular file.
type settings struct {
	ConfigDir string `json:"configDir"`
	HostName  string `json:"hostName"`

	// Target files, relative to ConfigDir. Empty = look them up as before.
	// If they name a file that does not exist yet, the first run as root
	// creates it and adds it to the imports list.
	PackagesFile string `json:"packagesFile"`
	ServicesFile string `json:"servicesFile"`
}

// dropEscapingTargets discards target files that point outside the
// configuration. config.json is edited by hand and is not validated; a
// "../elsewhere.nix" would silently land outside via filepath.Join, and
// validName never sees these fields. Empty means: look them up as if nothing
// had been configured.
func (s *settings) dropEscapingTargets() {
	for _, f := range []*string{&s.PackagesFile, &s.ServicesFile} {
		if *f != "" && !filepath.IsLocal(*f) {
			fmt.Fprintf(os.Stderr, "nixpkg: %q points outside the configuration -- ignoring it\n", *f)
			*f = ""
		}
	}
}

// cfg is the stored answer. Empty as long as nothing has been set up -- then
// nixpkg behaves as before and looks for the files itself.
var cfg settings

// configCandidates are the usual places for a Flake configuration. Only used
// as a suggestion in the questionnaire -- nothing is ever guessed.
var configCandidates = []string{"/etc/nixos", "/nixos-config"}

// applySettings fills configDir and hostName from the stored answer.
// Explicitly given flags always win: whoever passes -config means exactly that
// configuration, even if a different one is stored.
func applySettings() {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["config"] && set["host"] {
		return
	}

	s, err := readSettings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixpkg: cannot read stored settings (%v) -- running setup again\n", err)
		s = nil
	}
	if s == nil {
		// No terminal (script, CI, pipe) means no questions. A questionnaire
		// nobody can answer would hang the run. An explicit -config or -host
		// means the same: the questionnaire would default its suggestions to
		// /etc/nixos and can fail outright ("no Flake here") even though the
		// flag already points at a perfectly good configuration elsewhere --
		// exactly the "-config /tmp/cfg-test" trial run the README recommends.
		if !setupNeeded(set, interactive()) {
			return
		}
		s = askSettings()
		if err := saveSettings(s); err != nil {
			fmt.Fprintf(os.Stderr, "nixpkg: settings not saved (%v) -- setup will run again next time\n", err)
		}
	}
	s.dropEscapingTargets()
	cfg = *s
	if !set["config"] && s.ConfigDir != "" {
		configDir = s.ConfigDir
	}
	if !set["host"] && s.HostName != "" {
		hostName = s.HostName
	}
}

// setupNeeded reports whether the interactive questionnaire should run: only
// with a terminal that can actually answer, and only when the user has not
// already told nixpkg where to look via -config/-host -- asking anyway would
// override that intent with a guess.
func setupNeeded(set map[string]bool, interactive bool) bool {
	return interactive && !set["config"] && !set["host"]
}

// askSettings is the one-time setup.
func askSettings() *settings {
	fmt.Println("nixpkg is not set up yet -- a few questions, just once.")
	fmt.Println("(Enter accepts the suggestion in square brackets.)")
	fmt.Println()

	in := bufio.NewScanner(os.Stdin)
	s := &settings{ConfigDir: ask(in, "Path to your NixOS configuration", detectConfigDir())}

	if _, err := os.Stat(filepath.Join(s.ConfigDir, "configuration.nix")); err != nil {
		fmt.Fprintf(os.Stderr, "\nNote: there is no readable configuration.nix in %s.\n\n", s.ConfigDir)
	}

	// Has to come early: without a Flake, nixpkg cannot build, search or
	// update anything, and every further question would be pointless.
	if !askYes(in, "Do you use Flakes?", hasFlake(s.ConfigDir)) {
		die("nixpkg requires a Flake-based NixOS configuration.\n" +
			"       Nothing was saved and nothing was changed.")
	}
	s.HostName = ask(in, "Host name in the Flake (nixosConfigurations.<name>)", detectHostName(s.ConfigDir))

	// Target files. The suggestions follow what is already there -- pressing
	// Enter changes nothing about the existing configuration.
	fmt.Println()
	if askYes(in, "Put packages in a file of their own?", !hasPackageList(s.ConfigDir, "configuration.nix")) {
		s.PackagesFile = ask(in, "  File for packages", "modules/packages.nix")
	} else {
		s.PackagesFile = "configuration.nix"
	}

	if askYes(in, "Put module switches (programs.*.enable) in that same file?", true) {
		s.ServicesFile = s.PackagesFile
	} else {
		s.ServicesFile = ask(in, "  File for module switches", "modules/services.nix")
	}

	if miss := missingFiles(s); len(miss) > 0 {
		verb, obj := "does not exist yet", "it"
		if len(miss) > 1 {
			verb, obj = "do not exist yet", "them"
		}
		fmt.Printf("\n%s %s -- the first run with sudo will create %s\n", strings.Join(miss, " and "), verb, obj)
		fmt.Printf("and add %s to the imports list in configuration.nix.\n", obj)
	}
	fmt.Println()
	return s
}

// missingFiles names the configured target files that do not exist yet -- each
// only once, even when packages and switches share the same file.
func missingFiles(s *settings) []string {
	var out []string
	for _, rel := range []string{s.PackagesFile, s.ServicesFile} {
		if rel == "" || slices.Contains(out, rel) {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.ConfigDir, rel)); err != nil {
			out = append(out, rel)
		}
	}
	return out
}

// askYes asks a yes/no question. German answers stay accepted because an
// earlier version asked in German -- a stored habit should not start failing.
func askYes(in *bufio.Scanner, question string, suggestion bool) bool {
	hint := "y/N"
	if suggestion {
		hint = "Y/n"
	}
	for {
		fmt.Printf("%s [%s]: ", question, hint)
		if !in.Scan() {
			fmt.Println()
			return suggestion
		}
		switch strings.ToLower(strings.TrimSpace(in.Text())) {
		case "":
			return suggestion
		case "y", "yes", "j", "ja":
			return true
		case "n", "no", "nein":
			return false
		}
		fmt.Println("  Please answer y or n.")
	}
}

// hasFlake is the suggestion for the Flake question.
func hasFlake(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "flake.nix"))
	return err == nil
}

// hasPackageList reports whether rel already holds a systemPackages list. Only
// used for the suggestion: whoever already keeps packages in configuration.nix
// gets "no" suggested, everyone else "yes".
func hasPackageList(dir, rel string) bool {
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return false
	}
	lists, err := pkgLists(strings.Split(string(b), "\n"))
	return err == nil && len(lists) > 0
}

func ask(in *bufio.Scanner, question, suggestion string) string {
	fmt.Printf("%s [%s]: ", question, suggestion)
	if !in.Scan() {
		// EOF in the middle of the questionnaire (Ctrl-D): take the suggestion
		// instead of aborting.
		fmt.Println()
		return suggestion
	}
	if answer := strings.TrimSpace(in.Text()); answer != "" {
		return answer
	}
	return suggestion
}

// detectConfigDir takes the first candidate that holds a flake.nix.
func detectConfigDir() string {
	for _, dir := range configCandidates {
		if _, err := os.Stat(filepath.Join(dir, "flake.nix")); err == nil {
			return dir
		}
	}
	return configCandidates[0]
}

// detectHostName works out the Flake attribute name the system is built under.
//
// The system's host name is only a hint: it is often, but not necessarily, the
// same as the attribute name, and a wrong name only surfaces at rebuild time
// -- after the lock file has already been updated. So the Flake itself is
// asked which configurations it knows about.
//
// Exactly one -- then the answer is unambiguous. If the system name is among
// them, that is it. Otherwise the available names are printed, so nobody has
// to guess.
func detectHostName(dir string) string {
	sys, _ := os.Hostname()

	names := flakeHosts(dir)
	if len(names) == 0 {
		// No Flake reachable (not built yet, nix slow, an error in the Flake):
		// the system name is then the best guess available.
		if sys != "" {
			return sys
		}
		return hostName
	}
	if len(names) == 1 {
		return names[0]
	}
	if slices.Contains(names, sys) {
		fmt.Printf("  in the Flake: %s\n", strings.Join(names, ", "))
		return sys
	}
	fmt.Printf("  the Flake offers: %s\n", strings.Join(names, ", "))
	if sys != "" {
		fmt.Printf("  (the system name %q is not among them)\n", sys)
	}
	return names[0]
}

// flakeHosts reads the names under nixosConfigurations. Empty if that fails
// for any reason -- setup must not break over it, it only gets better with it.
func flakeHosts(dir string) []string {
	if !hasFlake(dir) {
		return nil
	}
	fmt.Println("  (asking the Flake which configurations it has...)")

	// With a time limit: a cold evaluation can take forever, and nobody should
	// be left sitting in front of a hanging questionnaire.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "nix", "eval", "--json",
		dir+"#nixosConfigurations", "--apply", "builtins.attrNames").Output()
	if err != nil {
		return nil
	}
	var names []string
	if err := json.Unmarshal(out, &names); err != nil {
		return nil
	}
	sort.Strings(names)
	return names
}

func interactive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// ── Where the answers live ──────────────────────────────────────────────────

// settingsPath returns the file holding the answers -- always in the real
// user's home, never in root's.
//
// This is the crux: setup runs while searching without sudo, the first install
// afterwards runs with sudo. If this used os.UserConfigDir(), the answer would
// sit in ~/.config and be looked for in /root/.config -- nixpkg would ask
// again every time and let two states drift apart. Whether sudo passes HOME
// through depends on env_reset/env_keep in sudoers and is nothing to rely on;
// SUDO_USER, by contrast, is always there.
func settingsPath() (string, error) {
	base, err := userConfigHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "nixpkg", "config.json"), nil
}

func userConfigHome() (string, error) {
	su := os.Getenv("SUDO_USER")
	if su == "" || os.Geteuid() != 0 {
		return os.UserConfigDir()
	}
	u, err := user.Lookup(su)
	if err != nil {
		return "", fmt.Errorf("user %q not found: %w", su, err)
	}
	// A fixed ~/.config for the sudo case. A user's differing XDG_CONFIG_HOME
	// is not visible here -- sudo clears it away. Only worth handling once it
	// actually bites someone.
	return filepath.Join(u.HomeDir, ".config"), nil
}

// settingsPathOrName exists only for messages: a path when one can be worked
// out, otherwise the bare file name.
func settingsPathOrName() string {
	if p, err := settingsPath(); err == nil {
		return p
	}
	return "config.json"
}

// readSettings returns (nil, nil) when nothing has been stored yet.
func readSettings() (*settings, error) {
	path, err := settingsPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s settings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func saveSettings(s *settings) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}
	chownToCaller(path)
	fmt.Printf("Saved to %s -- change it there or with -config/-host.\n", path)
	return nil
}

// chownToCaller hands a file created under sudo to the real user. Otherwise
// their own ~/.config/nixpkg belongs to root, and the next run without sudo
// cannot write to it any more.
func chownToCaller(path string) {
	if os.Geteuid() != 0 {
		return
	}
	su := os.Getenv("SUDO_USER")
	if su == "" {
		return
	}
	u, err := user.Lookup(su)
	if err != nil {
		return
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return
	}
	if err := os.Chown(path, uid, gid); err != nil {
		fmt.Fprintf(os.Stderr, "nixpkg: %s still belongs to root (%v)\n", path, err)
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.Chown(dir, uid, gid)
	}
}
