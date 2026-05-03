# punch

Copy-based dotfile manager with provenance tracking. Drop-in replacement for [rotz](https://github.com/volllly/rotz) that copies files instead of symlinking them.

## Why

Symlinks break in git worktrees. When you delete a worktree, symlinks pointing into it go dangling. Copy-based installs are self-contained -- the source can be deleted afterward.

## Usage

```bash
# Status: what's current, what's drifted
punch status

# Link: copy dotfiles to their targets
punch link              # copy, skip conflicts
punch link --force      # overwrite even if target was modified
punch link --dry-run    # preview without copying

# Install: run install commands (respects depends)
punch install

# Diff: compare source vs installed target
punch diff ~/.gitconfig

# Clean: remove orphaned lockfile entries
punch clean
```

## Config format

Uses the same `dot.yaml` format as rotz. Files are discovered by walking the dotfiles directory for `dot.yaml` files.

```yaml
# Platform scoping
global:
  files:
    .gitconfig: ~/.gitconfig
  installs: "some command"

darwin:
  files:
    mac-only.conf: ~/.config/thing/config
  installs: "brew install thing"
  depends:
    - ../other/module

linux:
  files:
    linux-only.conf: ~/.config/thing/config
```

The `links:` key is accepted as an alias for `files:` (rotz compatibility).

Top-level keys without platform scope are treated as global:

```yaml
files:
  .zshrc: ~/.zshrc

installs: "brew install zsh"
```

## Provenance

Every copy is recorded in `~/.local/state/punch/lock.json`:

```json
{
  "files": {
    "/Users/you/.gitconfig": {
      "source": "/path/to/dotfiles/dev/git/.gitconfig",
      "source_hash": "abc123...",
      "target_hash": "abc123...",
      "installed_at": "2026-04-04T...",
      "module": "dev/git"
    }
  }
}
```

This enables:
- **Drift detection**: know when targets were modified outside punch
- **Source tracking**: know when source files were updated
- **Conflict resolution**: don't overwrite user edits without `--force`
- **Orphan detection**: find lockfile entries whose source modules were removed

## Auto-detection

punch walks up from cwd looking for `config.yaml` to find the dotfiles root. Override with `--dotfiles <path>` or `PUNCH_DOTFILES` env var.

## Building

```bash
go build -o punch .
cp punch ~/.local/bin/
```
