package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// validName limits what can ever be written into a .nix file or into a
// command. Anything else is rejected before it gets anywhere.
var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]*$`)

// importsDecl matches the line that opens the imports list -- "imports" as a
// word of its own followed by an "=", so that neither a comment nor an
// identically named attribute of some other attrset triggers it.
var importsDecl = regexp.MustCompile(`(^|[^A-Za-z0-9._-])imports\s*=`)

// nixpkgMarker labels the block nixpkg owns. It lives in the user's .nix files
// and is read back on every run -- changing it would orphan every entry ever
// written.
const nixpkgMarker = "# via nixpkg"

// noopError means: there is nothing to do here (the package is already listed,
// or is not listed at all). That is no reason to abort a run covering several
// packages and roll back what has already been written -- main skips such
// cases and carries on with the next package.
type noopError struct{ msg string }

func (e *noopError) Error() string { return e.msg }

func noopf(format string, a ...any) error {
	return &noopError{fmt.Sprintf(format, a...)}
}

func isNoop(err error) bool {
	var n *noopError
	return errors.As(err, &n)
}

// ── Package list ────────────────────────────────────────────────────────────

// addPackage inserts name into the "# via nixpkg" block of the package list,
// alphabetically. If the block does not exist yet, it is created before the
// list's closing "];".
func addPackage(src, name string) (string, error) {
	if has, _ := hasPackage(src, name); has {
		return "", noopf("%s is already in the package list", name)
	}
	lines := strings.Split(src, "\n")
	lists, err := pkgLists(lines)
	if err != nil {
		return "", err
	}
	if len(lists) == 0 {
		return "", fmt.Errorf("environment.systemPackages not found")
	}
	// Always the first list in the file: writing into a second one would be
	// arbitrary, and both are equally valid.
	list := lists[0]
	indent := entryIndent(lines, list)

	marker := -1
	for i := list[0] + 1; i < list[1]; i++ {
		if strings.TrimSpace(lines[i]) == nixpkgMarker {
			marker = i
			break
		}
	}

	if marker == -1 {
		block := []string{"", indent + nixpkgMarker, indent + name}
		return strings.Join(insert(lines, list[1], block...), "\n"), nil
	}

	// Sort alphabetically from the marker to the end of the block. The block
	// ends at the first line that is not a package -- a blank line, a comment
	// (also blank after stripComment) or the end of the list.
	at := marker + 1
	for at < list[1] {
		t := strings.TrimSpace(stripComment(lines[at]))
		if t == "" || strings.HasPrefix(t, "]") {
			break
		}
		if t > name {
			break
		}
		at++
	}
	return strings.Join(insert(lines, at, indent+name), "\n"), nil
}

// removePackage deletes every line of the package list that is exactly name --
// no matter which section it sits in, including hand-maintained ones. Cleans
// up a nixpkg block that has become empty along the way.
func removePackage(src, name string) (string, error) {
	if has, _ := hasPackage(src, name); !has {
		return "", noopf("%s is not in the package list", name)
	}
	// A loop, because a package may accidentally be listed twice. Leaving one
	// of them behind would mean: reported as "removed", still installed.
	for {
		has, idx := hasPackage(src, name)
		if !has {
			return src, nil
		}
		lines := strings.Split(src, "\n")
		lines = append(lines[:idx:idx], lines[idx+1:]...)
		src = strings.Join(dropEmptyMarker(lines), "\n")
	}
}

// hasPackage looks for name as a line of its own -- and only inside an
// environment.systemPackages list. The same files contain identical-looking
// lines elsewhere (fonts.packages, programs.nix-ld.libraries); those must
// neither count as "already installed" nor be deleted.
func hasPackage(src, name string) (bool, int) {
	has, idx, _ := findPackage(src, name)
	return has, idx
}

// findPackage is hasPackage with an error. A file whose package list cannot be
// read unambiguously must not pass as "the package is not in there" -- that is
// a different statement, and the caller has to be able to make it.
func findPackage(src, name string) (bool, int, error) {
	lines := strings.Split(src, "\n")
	lists, err := pkgLists(lines)
	if err != nil {
		return false, -1, err
	}
	for _, list := range lists {
		for i := list[0] + 1; i < list[1]; i++ {
			if strings.TrimSpace(stripComment(lines[i])) == name {
				return true, i, nil
			}
		}
	}
	return false, -1, nil
}

// pkgLists returns the line ranges of every environment.systemPackages list in
// the file: {line holding the option, line holding the closing bracket}.
// NixOS allows environment.systemPackages to appear more than once, even in
// the same file.
func pkgLists(lines []string) ([][2]int, error) {
	var out [][2]int
	for i := 0; i < len(lines); i++ {
		// stripComment: a commented-out list is not a list.
		if !strings.Contains(stripComment(lines[i]), "environment.systemPackages") {
			continue
		}
		end, err := listEnd(lines, i, "environment.systemPackages")
		if err != nil {
			return nil, err
		}
		out = append(out, [2]int{i, end})
		i = end
	}
	return out, nil
}

// listEnd finds the line holding the closing bracket for the option starting
// at start -- by bracket depth, not by the first "];". A nested list would
// otherwise end the list early and put the new entry into the middle of an
// unrelated expression.
// what names the list in the error messages (systemPackages or imports).
func listEnd(lines []string, start int, what string) (int, error) {
	depth, opened := 0, false
	for i := start; i < len(lines); i++ {
		for _, c := range stripComment(lines[i]) {
			switch c {
			case '[':
				depth++
				opened = true
			case ']':
				depth--
			}
		}
		if !opened || depth > 0 {
			continue
		}
		if depth < 0 {
			return 0, fmt.Errorf("unbalanced brackets in %s (line %d)", what, i+1)
		}
		if i == start {
			return 0, fmt.Errorf("%s is written entirely on line %d -- please spread it over several lines", what, i+1)
		}
		// When in doubt, stop rather than guess: if the closing line carries
		// anything else, it is unclear where an entry belongs.
		if t := strings.TrimSpace(stripComment(lines[i])); t != "]" && t != "];" {
			return 0, fmt.Errorf("end of %s on line %d is ambiguous: %q", what, i+1, t)
		}
		return i, nil
	}
	return 0, fmt.Errorf("end of %s not found", what)
}

// entryIndent adopts the indentation of the existing entries instead of
// assuming four spaces.
func entryIndent(lines []string, list [2]int) string {
	for i := list[0] + 1; i < list[1]; i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			continue
		}
		return l[:len(l)-len(strings.TrimLeft(l, " \t"))]
	}
	return "    "
}

// dropEmptyMarker removes the nixpkg block once no package is left underneath.
// "No package" also means: a comment line follows (the user's next section) or
// the end of the list or of the attrset.
func dropEmptyMarker(lines []string) []string {
	for i, l := range lines {
		if strings.TrimSpace(l) != nixpkgMarker {
			continue
		}
		next := ""
		if i+1 < len(lines) {
			next = strings.TrimSpace(stripComment(lines[i+1]))
		}
		if next != "" && !strings.HasPrefix(next, "]") && !strings.HasPrefix(next, "}") {
			return lines // there is still a package underneath
		}
		cut := i
		if cut > 0 && strings.TrimSpace(lines[cut-1]) == "" {
			cut-- // take the blank line above along
		}
		return append(lines[:cut:cut], lines[i+1:]...)
	}
	return lines
}

// ── Module switches ─────────────────────────────────────────────────────────

// setService writes or removes a "<path> = true;" line.
// path is e.g. "programs.steam.enable".
func setService(src, path string, on bool) (string, error) {
	lines := strings.Split(src, "\n")
	set := regexp.MustCompile(`^` + regexp.QuoteMeta(path) + `\s*=\s*(true|false)\s*;$`)

	for i, l := range lines {
		t := strings.TrimSpace(stripComment(l))
		if m := set.FindStringSubmatch(t); m != nil {
			switch {
			case on && m[1] == "true":
				return "", noopf("%s is already enabled", path)
			case on:
				// Currently false: flip it. Adding a second line would be an
				// "attribute already defined" at rebuild time.
				lines[i] = strings.Replace(l, "false", "true", 1)
				return strings.Join(lines, "\n"), nil
			case m[1] == "false":
				return "", noopf("%s is not enabled", path)
			default:
				return strings.Join(append(lines[:i:i], lines[i+1:]...), "\n"), nil
			}
		}
		// The same option written differently ("programs.steam = { ... }" or
		// "= lib.mkDefault true;"). Nothing may be appended and nothing
		// deleted there -- better to stop with a clear message.
		if definesPrefix(t, path) {
			return "", fmt.Errorf("%s is already set differently on line %d (%q) -- please change it by hand", path, i+1, t)
		}
	}
	if !on {
		return "", noopf("%s is not enabled", path)
	}

	// Insert before the last closing brace.
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "}" {
			return strings.Join(insert(lines, i, "  "+path+" = true;"), "\n"), nil
		}
	}
	return "", fmt.Errorf("no closing } found")
}

// definesOption reports whether the file sets path at all (or a prefix of it
// as an attrset). This is how the module that actually holds a switch is
// found, instead of assuming a fixed file.
func definesOption(src, path string) bool {
	for _, l := range strings.Split(src, "\n") {
		if definesPrefix(strings.TrimSpace(stripComment(l)), path) {
			return true
		}
	}
	return false
}

// definesPrefix recognises "programs.steam.enable =", but also the equivalent
// "programs.steam =" followed by an attrset.
func definesPrefix(line, path string) bool {
	parts := strings.Split(path, ".")
	for n := len(parts); n > 0; n-- {
		rest := strings.TrimPrefix(line, strings.Join(parts[:n], "."))
		if rest == line {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(rest), "=") {
			return true
		}
	}
	return false
}

// ── Odds and ends ───────────────────────────────────────────────────────────

func insert(lines []string, at int, vals ...string) []string {
	out := make([]string, 0, len(lines)+len(vals))
	out = append(out, lines[:at]...)
	out = append(out, vals...)
	return append(out, lines[at:]...)
}

func stripComment(l string) string {
	if i := strings.Index(l, "#"); i >= 0 {
		return l[:i]
	}
	return l
}

// ── imports list ────────────────────────────────────────────────────────────

// addImport adds rel (e.g. "modules/packages.nix") to the imports list of
// configuration.nix. Without that entry a freshly created file is invisible to
// NixOS -- nixpkg would dutifully write into it and nothing would happen.
//
// If the imports list is missing entirely it is NOT created: where it belongs
// depends on how the file is built, and guessing inside someone else's system
// configuration is the wrong place to take initiative.
func addImport(src, rel string) (string, error) {
	entry := "./" + rel
	lines := strings.Split(src, "\n")

	start := -1
	for i, l := range lines {
		// Word boundary: plain "imports" would otherwise also catch neighbours
		// like "nixpkgs.imports" or a comment above it.
		if importsDecl.MatchString(stripComment(l)) {
			start = i
			break
		}
	}
	if start == -1 {
		return "", fmt.Errorf("no imports list found -- please add %q by hand", entry)
	}

	end, err := listEnd(lines, start, "imports")
	if err != nil {
		return "", err
	}

	for i := start; i <= end; i++ {
		for _, m := range importPath.FindAllStringSubmatch(stripComment(lines[i]), -1) {
			if m[1] == rel {
				return "", noopf("%s is already in imports", entry)
			}
		}
	}

	return strings.Join(insert(lines, end, entryIndent(lines, [2]int{start, end})+entry), "\n"), nil
}
