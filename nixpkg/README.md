# nixpkg

Manage packages in a NixOS Flake configuration without editing `packages.nix`
by hand and typing the rebuild command every time.

```
nixpkg -s cava              search (no sudo needed)
sudo nixpkg -i cava steam   add + rebuild
sudo nixpkg -r cava         remove + rebuild + collect garbage
sudo nixpkg -u              update all Flake inputs + rebuild
```

> **Status:** works on the system it was built for. It has not been tried on
> anyone else's configuration yet. It writes into your NixOS configuration —
> read the dry run once before you put `sudo` in front of it.

## Without sudo everything is a dry run

`-i`, `-r` and `-u` change nothing without root and only show the planned diff:

```
$ nixpkg -i cava
== Dry run (no root) -- nothing will be changed ==

install cava -> modules/packages.nix
  + # via nixpkg
  + cava
```

## First-run setup

On the first run nixpkg asks for what it cannot reliably guess. Enter accepts
the suggestion.

```
Path to your NixOS configuration [/etc/nixos]:
Do you use Flakes? [Y/n]:
Host name in the Flake (nixosConfigurations.<name>) [mypc]:
Put packages in a file of their own? [y/N]:
Put module switches (programs.*.enable) in that same file? [Y/n]:
```

The answers go to `~/.config/nixpkg/config.json` and can be overridden there or
with `-config` / `-host`.

### Flakes are a requirement

Answer "no" and nixpkg stops, rather than pretending it can cope. Search,
rebuild and update all go through Flake commands (`nix search nixpkgs`,
`nixos-rebuild --flake`, `nix flake update`), and `nix flake update` shares
neither its lock model nor its rollback path with `nix-channel --update`. With
channels this would be a different program.

### The host name

A wrong host name would otherwise only surface at rebuild time — after the lock
file has already been updated. So the Flake itself is asked what it knows:

```
nix eval --json /etc/nixos#nixosConfigurations --apply builtins.attrNames
```

If there is exactly one configuration, that is the answer. If the system's host
name is among them, it is suggested. Otherwise nixpkg prints the available
names so nobody has to guess. If the Flake does not answer within 15 seconds,
or not at all, the system host name stays as the suggestion.

## What the two layouts look like

### Everything in one file

"Put packages in a file of their own?" answered with **no**. nixpkg writes
straight into `configuration.nix`:

```nix
# configuration.nix
{ config, pkgs, ... }:
{
  imports = [ ./hardware-configuration.nix ];

  programs.steam.enable = true;        # <- module switch

  environment.systemPackages = with pkgs; [
    git

    # via nixpkg
    cava                               # <- new packages
    ripgrep
  ];

  system.stateVersion = "26.05";
}
```

New packages go alphabetically into the `# via nixpkg` block. Everything above
it stays untouched — nixpkg never writes into your own sections.

### Separate files

"Put packages in a file of their own?" answered with **yes**, and the module
switches into a second one. On the first run with `sudo`, nixpkg creates the
files and adds them to `imports` itself:

```nix
# configuration.nix
{ config, pkgs, ... }:
{
  imports = [
    ./hardware-configuration.nix
    ./modules/packages.nix             # <- added by nixpkg
    ./modules/services.nix             # <- added by nixpkg
  ];

  system.stateVersion = "26.05";
}
```

```nix
# modules/packages.nix
# created by nixpkg
{ pkgs, ... }:
{
  environment.systemPackages = with pkgs; [
    # via nixpkg
    cava
    ripgrep
  ];
}
```

```nix
# modules/services.nix
# created by nixpkg
{ ... }:
{
  programs.steam.enable = true;
}
```

If you do *not* want the switches kept separately, answer "module switches in
that same file?" with yes — then `programs.*.enable` and the package list end
up together in `modules/packages.nix`.

### What happens when something goes wrong

Creation happens in this order: the module file first, then the `imports`
entry. The other way round, `configuration.nix` would briefly point at a file
that does not exist — and aborting exactly in between would leave a system that
no longer builds. If the rebuild fails, the created file is deleted again and
`configuration.nix` is restored from its backup.

## What it does when writing

- **A backup before every change** (`.bak`), rolled back on failure
- **A git commit beforehand**, if the configuration is a repository
- **`git add`** before the rebuild — a Flake only sees files that Git knows
- **A lock** against two concurrent runs on the same configuration
- With several packages in one run, the state is carried per file instead of
  being re-read from disk each time

## Package or module?

Some packages need their NixOS module because it does more than drop a binary
in place — drivers, udev rules, firewall, wrappers. For those nixpkg writes
`programs.<name>.enable = true` instead of a package line. The list lives as
`serviceModules` in `main.go` (steam, hyprland, gamemode, gamescope, wireshark,
virt-manager, thunar, dconf, nix-ld, adb).

Everything else becomes a plain package line, even when a module of the same
name happens to exist — `programs.firefox`, for instance, only sets up
enterprise policies, which is usually not what anyone wants.

You can force either direction:

```
sudo nixpkg -service <name>   treat it as a module
sudo nixpkg -package <name>   treat it as a package
```

## Other flags

```
-a         with -s, also show libraries and language packages
-v         print the commands being run
-config    use a different configuration than the stored one
-host      use a different host name than the stored one
```

## Building and testing

```sh
go build -o nixpkg .
go test ./...
```

No dependencies beyond the standard library.

To try it out without any risk: make a copy of your configuration and point
`-config` at it. Without root it stays a dry run anyway.

```sh
cp -r /etc/nixos /tmp/cfg-test
./nixpkg -config /tmp/cfg-test -i cava
```
