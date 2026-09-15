package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// runOut captures stdout only. run() would be wrong here: it mixes in stderr,
// and nix writes its progress ("evaluating ...") there -- that would end up in
// the middle of the JSON.
func runOut(name string, args ...string) (string, error) {
	trace(name, args)
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return string(out), err
}

// Three quarters of all search results are libraries, language packages and
// plugins -- things nobody ever puts into systemPackages by hand. nix also
// searches descriptions, so a query for "flatp" drags in everything that
// mentions the word anywhere in its text.
//
// A hit is shown when one of three rules applies:
//
//  1. The name is exactly the search term -- always, no exceptions.
//  2. meta.mainProgram is set: the package definitely ships a program. Inside
//     a language ecosystem (haskellPackages and friends) that alone is not
//     enough, the name has to match as well -- otherwise every search washes
//     up other people's example projects.
//  3. The name itself matches the search term. A "lib" prefix without a
//     mainProgram still stays out.
//
// Everything else is a description-only hit with no demonstrable program --
// exactly the noise to be removed. -a shows everything again.
//
// On ordering: python3Packages is on the blocklist, yet
// python3Packages.python-lsp-server survives a search for "python-lsp"
// because both name and mainProgram match. libreoffice survives the "lib"
// prefix for the same reason.
//
// kdePackages is missing from the blocklist on purpose: it holds real
// applications (dolphin, gwenview), and gwenview does not even have a
// mainProgram -- a blanket rule would make it disappear even though it is
// installed.
var namespaceExceptions = map[string]bool{"kdePackages": true}

// isLanguageNamespace recognises language ecosystems by their suffix instead
// of listing them: python313Packages, python314Packages and whatever else
// nixpkgs ships would always be one release behind as a hardcoded list.
func isLanguageNamespace(ns string) bool {
	if namespaceExceptions[ns] {
		return false
	}
	return strings.HasSuffix(ns, "Packages") ||
		strings.HasSuffix(ns, "Plugins") ||
		strings.HasSuffix(ns, "Extensions")
}

type searchHit struct {
	Description string `json:"description"`
	Pname       string `json:"pname"`
	Version     string `json:"version"`
}

type result struct {
	attr        string // attribute path without legacyPackages.<system>.
	hit         searchHit
	mainProgram string
	rank        int
}

// searchPackages searches and shows only what looks like a program.
// showAll turns the filter off.
func searchPackages(terms []string, showAll bool) {
	args := append([]string{"search", "nixpkgs", "--json", "--"}, terms...)
	out, err := runOut("nix", args...)
	if err != nil {
		die("search failed: %v", err)
	}

	var raw map[string]searchHit
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		die("cannot read search result: %v", err)
	}
	if len(raw) == 0 {
		fmt.Println("nothing found")
		return
	}

	results := make([]*result, 0, len(raw))
	attrs := make([]string, 0, len(raw))
	for full, hit := range raw {
		attr := full
		// "legacyPackages.x86_64-linux.foo" -> "foo"; the first two segments
		// are the same for every hit and carry no information.
		if parts := strings.SplitN(full, ".", 3); len(parts) == 3 && parts[0] == "legacyPackages" {
			attr = parts[2]
		}
		results = append(results, &result{attr: attr, hit: hit})
		attrs = append(attrs, attr)
	}

	// Fixed order before anything is filtered: iterating a Go map is random,
	// and the dedup below would otherwise keep a different generation on every
	// run. Shorter namespace first, so python3Packages wins over
	// python314Packages.
	sort.Slice(results, func(i, j int) bool {
		ni, nj := nsOf(results[i].attr), nsOf(results[j].attr)
		if len(ni) != len(nj) {
			return len(ni) < len(nj)
		}
		return results[i].attr < results[j].attr
	})

	// A single eval pass for all hits. If it fails, mainProgram stays empty --
	// then only rule 2 filters, instead of the whole search dying.
	programs := mainPrograms(attrs)
	for _, r := range results {
		r.mainProgram = programs[r.attr]
	}

	needles := make([]string, 0, len(terms))
	for _, t := range terms {
		needles = append(needles, strings.ToLower(strings.Trim(t, "^$")))
	}

	shown := make([]*result, 0, len(results))
	hidden := 0
	for _, r := range results {
		// The last attribute segment is compared, not pname: for
		// python313Packages.python-lsp-server the pname is
		// "python3.13-python-lsp-server", and nobody searches for that.
		name := strings.ToLower(baseName(r.attr))
		exact, namePrefix, nameHit := false, false, false
		for _, n := range needles {
			if name == n {
				exact = true
			}
			if strings.HasPrefix(name, n) {
				namePrefix = true
			}
			if strings.Contains(name, n) {
				nameHit = true
			}
		}

		if !showAll && !exact && !keep(r.attr, name, namePrefix, nameHit, r.mainProgram != "") {
			hidden++
			continue
		}

		switch {
		case exact:
			r.rank = 0
		case nameHit && r.mainProgram != "":
			r.rank = 1
		case nameHit:
			r.rank = 2
		default:
			r.rank = 3
		}
		shown = append(shown, r)
	}

	// python313Packages.x and python314Packages.x are the same package in two
	// generations -- once is enough.
	if !showAll {
		seen := map[string]bool{}
		dedup := shown[:0]
		for _, r := range shown {
			if isLanguageNamespace(nsOf(r.attr)) {
				if seen[baseName(r.attr)] {
					hidden++
					continue
				}
				seen[baseName(r.attr)] = true
			}
			dedup = append(dedup, r)
		}
		shown = dedup
	}

	sort.Slice(shown, func(i, j int) bool {
		if shown[i].rank != shown[j].rank {
			return shown[i].rank < shown[j].rank
		}
		// Same rank: top-level entries first -- "steam" is the hit, not
		// python313Packages.steam.
		if ni, nj := nsOf(shown[i].attr) == "", nsOf(shown[j].attr) == ""; ni != nj {
			return ni
		}
		return shown[i].attr < shown[j].attr
	})

	// Column width from the longest label, but capped: a single outlier like
	// haskellPackages.monomer-flatpak-example should not push every other
	// line's description to the right.
	width := 0
	for _, r := range shown {
		if n := len(r.attr) + len(r.hit.Version) + 3; n > width && n <= 44 {
			width = n
		}
	}
	for _, r := range shown {
		label := r.attr
		if r.hit.Version != "" {
			label += " (" + r.hit.Version + ")"
		}
		fmt.Printf("%-*s  %s\n", width, label, oneLine(r.hit.Description))
	}

	if hidden > 0 {
		fmt.Printf("\n%d libraries and language packages hidden, -a shows everything\n", hidden)
	}
	if len(shown) == 0 {
		fmt.Println("nothing found that ships a program -- -a shows everything")
	}
}

// nsOf is the namespace of an attribute path, i.e. everything before the first
// dot -- "" for a top-level package.
func nsOf(attr string) string {
	if i := strings.Index(attr, "."); i > 0 {
		return attr[:i]
	}
	return ""
}

// baseName is the last segment of an attribute path --
// "python313Packages.python-lsp-server" -> "python-lsp-server".
func baseName(attr string) string {
	if i := strings.LastIndex(attr, "."); i >= 0 {
		return attr[i+1:]
	}
	return attr
}

// keep implements the three rules from the comment above (without rule 1,
// which lives at the call site, because an exact hit is never filtered).
func keep(attr, pname string, namePrefix, nameHit, hasProgram bool) bool {
	if isLanguageNamespace(nsOf(attr)) {
		// A match anywhere in the name is not enough here:
		// haskellPackages.monomer-flatpak-example contains "flatpak" and even
		// has a mainProgram, but it is an example project.
		return namePrefix && hasProgram
	}
	if strings.HasPrefix(pname, "lib") && !hasProgram {
		return false
	}
	return hasProgram || nameHit
}

// mainPrograms queries meta.mainProgram for all hits in a single nix call --
// one call per package would take ~0.3 s each. tryEval catches packages whose
// meta throws during evaluation (broken, wrong platform, unfree); without it a
// single such package would take down the whole query.
func mainPrograms(attrs []string) map[string]string {
	var list strings.Builder
	list.WriteString("[ ")
	for _, a := range attrs {
		fmt.Fprintf(&list, "%q ", a)
	}
	list.WriteString("]")

	expr := `p:
let
  seg = n: builtins.filter builtins.isString (builtins.split "\\." n);
  pick = n: builtins.foldl' (a: s: a.${s}) p (seg n);
  get = n:
    let r = builtins.tryEval ((pick n).meta.mainProgram or "");
    in if r.success && builtins.isString r.value then r.value else "";
  names = ` + list.String() + `;
in builtins.listToAttrs (map (n: { name = n; value = get n; }) names)`

	out, err := runOut("nix", "eval", "--json", "nixpkgs#legacyPackages.x86_64-linux", "--apply", expr)
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return map[string]string{}
	}
	return m
}

// oneLine turns a multi-line description into a single short line.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 78
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}
