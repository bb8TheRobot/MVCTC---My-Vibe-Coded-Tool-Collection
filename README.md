# MVCTC — My Vibe-Coded Tool Collection

Small tools that exist because something on my own system annoyed me. One
folder per tool, each usable on its own, no shared framework.

| Tool | What it does |
|---|---|
| [nixpkg](nixpkg/) | Manage packages in a NixOS Flake configuration instead of editing `packages.nix` by hand |

## Installing

### Build it yourself

```sh
cd nixpkg
go build -o nixpkg .
```

No tool here has dependencies beyond the Go standard library, so a `go build`
is always enough.

### A prebuilt binary

```sh
chmod +x nixpkg
sudo install -m755 nixpkg /usr/local/bin/nixpkg
```

### On NixOS

There is no `/usr/local/bin` here. Either as a wrapper around the build in your
home directory:

```nix
environment.systemPackages = [
  (pkgs.writeShellScriptBin "nixpkg" ''exec /path/to/build/nixpkg "$@"'')
];
```

…or properly as a package, once the tool stops changing daily:

```nix
(pkgs.buildGoModule {
  pname = "nixpkg";
  version = "<version>";
  src = ./nixpkg;
  vendorHash = null;   # no dependencies beyond the stdlib
})
```

The wrapper is the more convenient route during development: one `go build` is
enough for every new state, no system rebuild per code change.

## Why "vibe-coded"

Because it is named after the way it came about. It is tested regardless — each
tool brings its own tests, and anything that touches the system does so with a
backup and a dry run.

## License

MIT, see [LICENSE](LICENSE).
