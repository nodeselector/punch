package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ── Config (dot.yaml) ──────────────────────────────────────────────

type DotConfig struct {
	Global *PlatformConfig `yaml:"global,omitempty"`
	Darwin *PlatformConfig `yaml:"darwin,omitempty"`
	Linux  *PlatformConfig `yaml:"linux,omitempty"`

	// Union platform keys (e.g. "linux|darwin") -- parsed in UnmarshalYAML
	UnionPlatform *PlatformConfig `yaml:"-"`

	// Top-level shorthand (no platform scope = global)
	Files   map[string]string `yaml:"files,omitempty"`
	Links   map[string]string `yaml:"links,omitempty"` // rotz compat alias
	Install any               `yaml:"installs,omitempty"`
	Depends []string          `yaml:"depends,omitempty"`
}

func (d *DotConfig) UnmarshalYAML(value *yaml.Node) error {
	// Decode known fields with a type alias to avoid recursion
	type plain DotConfig
	if err := value.Decode((*plain)(d)); err != nil {
		return err
	}

	// Scan for union platform keys like "linux|darwin" or "darwin|linux"
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			key := value.Content[i].Value
			if strings.Contains(key, "|") {
				parts := strings.Split(key, "|")
				for _, p := range parts {
					if strings.TrimSpace(p) == runtime.GOOS {
						var pc PlatformConfig
						if err := value.Content[i+1].Decode(&pc); err != nil {
							return err
						}
						d.UnionPlatform = &pc
						break
					}
				}
			}
		}
	}
	return nil
}

type PlatformConfig struct {
	Files   map[string]string `yaml:"files,omitempty"`
	Links   map[string]string `yaml:"links,omitempty"` // punch compat alias
	Install any               `yaml:"installs,omitempty"`
	Depends []string          `yaml:"depends,omitempty"`
}

// Merge platform-specific files with global. Platform overrides global.
func (d *DotConfig) ResolvedFiles() map[string]string {
	merged := make(map[string]string)

	// Global scope
	if d.Global != nil {
		for k, v := range d.Global.Files {
			merged[k] = v
		}
		for k, v := range d.Global.Links {
			merged[k] = v
		}
	}
	// Top-level shorthand
	for k, v := range d.Files {
		merged[k] = v
	}
	for k, v := range d.Links {
		merged[k] = v
	}

	// Platform scope
	pc := d.platformConfig()
	if pc != nil {
		for k, v := range pc.Files {
			merged[k] = v
		}
		for k, v := range pc.Links {
			merged[k] = v
		}
	}

	// Union platform scope (e.g. "linux|darwin")
	if d.UnionPlatform != nil {
		for k, v := range d.UnionPlatform.Files {
			merged[k] = v
		}
		for k, v := range d.UnionPlatform.Links {
			merged[k] = v
		}
	}

	return merged
}

func (d *DotConfig) ResolvedInstall() string {
	// Platform-specific install takes priority
	if pc := d.platformConfig(); pc != nil {
		if cmd := extractInstallCmd(pc.Install); cmd != "" {
			return cmd
		}
	}
	// Union platform
	if d.UnionPlatform != nil {
		if cmd := extractInstallCmd(d.UnionPlatform.Install); cmd != "" {
			return cmd
		}
	}
	if d.Global != nil {
		if cmd := extractInstallCmd(d.Global.Install); cmd != "" {
			return cmd
		}
	}
	return extractInstallCmd(d.Install)
}

func (d *DotConfig) ResolvedDepends() []string {
	var deps []string
	if d.Global != nil {
		deps = append(deps, d.Global.Depends...)
	}
	deps = append(deps, d.Depends...)
	if pc := d.platformConfig(); pc != nil {
		deps = append(deps, pc.Depends...)
	}
	if d.UnionPlatform != nil {
		deps = append(deps, d.UnionPlatform.Depends...)
	}
	return deps
}

func (d *DotConfig) platformConfig() *PlatformConfig {
	switch runtime.GOOS {
	case "darwin":
		return d.Darwin
	case "linux":
		return d.Linux
	}
	return nil
}

func extractInstallCmd(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		if val == "false" || val == "" {
			return ""
		}
		return val
	case bool:
		return ""
	case map[string]any:
		if cmd, ok := val["cmd"]; ok {
			if s, ok := cmd.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// ── Lockfile ───────────────────────────────────────────────────────

type Lockfile struct {
	Version int                    `json:"version"`
	Files   map[string]LockedFile  `json:"files"`
}

type LockedFile struct {
	Source      string `json:"source"`
	SourceHash  string `json:"source_hash"`
	TargetHash  string `json:"target_hash"`
	InstalledAt string `json:"installed_at"`
	Module      string `json:"module"`
}

func lockfilePath() string {
	return filepath.Join(stateDir(), "lock.json")
}

func stateDir() string {
	if dir := os.Getenv("PUNCH_STATE_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "punch")
}

func loadLockfile() *Lockfile {
	lf := &Lockfile{Version: 1, Files: make(map[string]LockedFile)}
	data, err := os.ReadFile(lockfilePath())
	if err != nil {
		return lf
	}
	_ = json.Unmarshal(data, lf)
	if lf.Files == nil {
		lf.Files = make(map[string]LockedFile)
	}
	return lf
}

func (lf *Lockfile) Save() error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(lockfilePath(), data, 0o644)
}

// ── Module discovery ───────────────────────────────────────────────

type Module struct {
	Name   string
	Dir    string
	Config DotConfig
}

func discoverModules(dotfilesDir string) ([]Module, error) {
	var modules []Module

	err := filepath.WalkDir(dotfilesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if d.Name() == ".git" || d.Name() == "node_modules" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "dot.yaml" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var cfg DotConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", path, err)
			return nil
		}

		dir := filepath.Dir(path)
		rel, _ := filepath.Rel(dotfilesDir, dir)
		modules = append(modules, Module{
			Name:   rel,
			Dir:    dir,
			Config: cfg,
		})
		return nil
	})

	return modules, err
}

// ── File operations ────────────────────────────────────────────────

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	if strings.HasPrefix(path, "$HOME/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[6:])
	}
	return path
}

func hashFile(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return hashDir(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

func hashDir(dir string) string {
	h := sha256.New()
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Name() == ".git" || d.Name() == "node_modules" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		fmt.Fprintf(h, "%s\n", rel)
		if !d.IsDir() {
			data, err := os.ReadFile(path)
			if err == nil {
				h.Write(data)
			}
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

func copyFile(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if srcInfo.IsDir() {
		return copyDir(src, dst)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func copyDir(src, dst string) error {
	// Remove existing target (might be a symlink)
	if info, err := os.Lstat(dst); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			os.Remove(dst)
		} else if info.IsDir() {
			os.RemoveAll(dst)
		}
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" || d.Name() == "node_modules" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		info, _ := d.Info()
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()

		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
		if err != nil {
			return err
		}
		defer out.Close()

		_, err = io.Copy(out, in)
		return err
	})
}

// ── Commands ───────────────────────────────────────────────────────

func cmdLink(dotfilesDir string, force bool, dryRun bool) error {
	modules, err := discoverModules(dotfilesDir)
	if err != nil {
		return err
	}

	lf := loadLockfile()
	copied := 0
	skipped := 0
	conflicts := 0

	for _, mod := range modules {
		files := mod.Config.ResolvedFiles()
		if len(files) == 0 {
			continue
		}

		headerShown := false
		showHeader := func() {
			if !headerShown {
				fmt.Printf("\n\033[1mLinking \033[38;5;4m/%s\033[39m\033[0m\n\n", mod.Name)
				headerShown = true
			}
		}

		for src, dst := range files {
			srcAbs := filepath.Join(mod.Dir, src)
			dstAbs := expandHome(dst)

			// Resolve symlinks at the target -- if it's a symlink (from old symlink-based installs),
			// we want to replace it with a copy. No conflict check needed.
			isSymlink := false
			if info, err := os.Lstat(dstAbs); err == nil && info.Mode()&os.ModeSymlink != 0 {
				isSymlink = true
				if !dryRun {
					os.Remove(dstAbs)
				}
			}

			// Check if source exists (file or dir)
			srcInfo, srcErr := os.Stat(srcAbs)
			if srcErr != nil {
				showHeader()
				fmt.Fprintf(os.Stderr, "  \033[31m✗ %s\033[0m  source not found\n", src)
				continue
			}

			srcHash := hashFile(srcAbs)
			if srcHash == "" {
				showHeader()
				fmt.Fprintf(os.Stderr, "  \033[31m✗ %s\033[0m  cannot hash source\n", src)
				continue
			}

			// For directories, also ensure target parent exists
			if srcInfo.IsDir() {
				if !dryRun {
					os.MkdirAll(filepath.Dir(dstAbs), 0o755)
				}
			}

			// Skip hash comparison for symlinks -- they're always replaced
			if !isSymlink {
				dstHash := hashFile(dstAbs)

				// Check if target is already up to date
				if dstHash == srcHash {
					skipped++
					continue
				}

				// Check for conflict: target exists and differs from both source and last-known
				if dstHash != "" && !force {
					if locked, ok := lf.Files[dstAbs]; ok {
						if dstHash != locked.TargetHash && dstHash != locked.SourceHash {
							showHeader()
							fmt.Printf("  \033[33m! %s\033[0m  modified outside punch (--force to overwrite)\n", dst)
							conflicts++
							continue
						}
					} else {
						showHeader()
						fmt.Printf("  \033[33m! %s\033[0m  exists, not in lockfile (--force to overwrite)\n", dst)
						conflicts++
						continue
					}
				}
			}

			if dryRun {
				showHeader()
				if isSymlink {
					fmt.Printf("  \033[33m↻\033[0m \033[38;5;2m%s\033[0m → \033[38;5;2m%s\033[0m  \033[2m(symlink → copy)\033[0m\n", src, dst)
				} else {
					fmt.Printf("  \033[38;5;2m%s\033[0m → \033[38;5;2m%s\033[0m\n", src, dst)
				}
				copied++
				continue
			}

			if err := copyFile(srcAbs, dstAbs); err != nil {
				showHeader()
				fmt.Fprintf(os.Stderr, "  \033[31m✗ %s\033[0m  %v\n", dst, err)
				continue
			}

			newHash := hashFile(dstAbs)
			lf.Files[dstAbs] = LockedFile{
				Source:      srcAbs,
				SourceHash:  srcHash,
				TargetHash:  newHash,
				InstalledAt: time.Now().Format(time.RFC3339),
				Module:      mod.Name,
			}

			showHeader()
			if isSymlink {
				fmt.Printf("  \033[33m↻\033[0m \033[38;5;2m%s\033[0m → \033[38;5;2m%s\033[0m  \033[2m(symlink → copy)\033[0m\n", src, dst)
			} else {
				fmt.Printf("  \033[38;5;2m%s\033[0m → \033[38;5;2m%s\033[0m\n", src, dst)
			}
			copied++
		}
	}

	if !dryRun {
		if err := lf.Save(); err != nil {
			return fmt.Errorf("saving lockfile: %w", err)
		}
	}

	fmt.Printf("\n%d copied, %d up-to-date, %d conflicts\n", copied, skipped, conflicts)
	return nil
}

func cmdInstall(dotfilesDir string) error {
	modules, err := discoverModules(dotfilesDir)
	if err != nil {
		return err
	}

	// Build module index for dependency resolution
	modIndex := make(map[string]*Module)
	for i := range modules {
		modIndex[modules[i].Name] = &modules[i]
	}

	// Topological sort based on depends
	visited := make(map[string]bool)
	var order []string
	var visit func(name string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		mod, ok := modIndex[name]
		if !ok {
			return
		}
		for _, dep := range mod.Config.ResolvedDepends() {
			// Resolve relative deps (../foo) against module's parent
			resolved := dep
			if strings.HasPrefix(dep, "../") || strings.HasPrefix(dep, "./") {
				resolved = filepath.Clean(filepath.Join(filepath.Dir(name), dep))
			}
			// Strip leading / if present (dot.yaml format uses /terminal/zsh)
			resolved = strings.TrimPrefix(resolved, "/")
			visit(resolved)
		}
		order = append(order, name)
	}

	for _, mod := range modules {
		visit(mod.Name)
	}

	installed := 0
	skipped := 0

	for _, name := range order {
		mod := modIndex[name]
		if mod == nil {
			continue
		}
		cmd := mod.Config.ResolvedInstall()
		if cmd == "" {
			continue
		}

		fmt.Printf("\n\033[1mInstalling \033[38;5;12m/%s\033[39m\033[0m\n\n", name)

		// Set DOTFILES_DIR for install scripts that reference it
		env := os.Environ()
		env = append(env, "DOTFILES_DIR="+dotfilesDir)

		proc := exec.Command("bash", "-c", cmd)
		proc.Dir = mod.Dir
		proc.Stdout = os.Stdout
		proc.Stderr = os.Stderr
		proc.Stdin = os.Stdin
		proc.Env = env

		if err := proc.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "  \033[31m✗\033[0m %v\n", err)
		} else {
			installed++
		}
	}

	if skipped > 0 {
		fmt.Printf("\n%d installed, %d skipped\n", installed, skipped)
	} else {
		fmt.Printf("\n%d installed\n", installed)
	}
	return nil
}

func cmdStatus(dotfilesDir string) error {
	modules, err := discoverModules(dotfilesDir)
	if err != nil {
		return err
	}

	lf := loadLockfile()

	// Track which locked files we've seen
	seen := make(map[string]bool)
	modified := 0
	current := 0
	missing := 0
	sourceChanged := 0

	symlinks := 0

	for _, mod := range modules {
		files := mod.Config.ResolvedFiles()
		for src, dst := range files {
			srcAbs := filepath.Join(mod.Dir, src)
			dstAbs := expandHome(dst)
			seen[dstAbs] = true

			srcHash := hashFile(srcAbs)
			dstHash := hashFile(dstAbs)

			if srcHash == "" {
				fmt.Printf("  \033[31m?\033[0m %s: source missing (%s)\n", dst, src)
				missing++
				continue
			}

			// Check if target is a symlink (legacy symlink)
			if info, err := os.Lstat(dstAbs); err == nil && info.Mode()&os.ModeSymlink != 0 {
				fmt.Printf("  \033[33m↻\033[0m %s: symlink (run link to convert)\n", dst)
				symlinks++
				continue
			}

			if dstHash == "" {
				fmt.Printf("  \033[33m-\033[0m %s: not installed\n", dst)
				missing++
				continue
			}

			locked, inLock := lf.Files[dstAbs]

			if dstHash == srcHash {
				current++
				continue
			}

			// Source changed since last install
			if inLock && srcHash != locked.SourceHash {
				fmt.Printf("  \033[34m↑\033[0m %s: source updated (%s)\n", dst, mod.Name)
				sourceChanged++
				continue
			}

			// Target modified outside punch
			if inLock && dstHash != locked.TargetHash {
				fmt.Printf("  \033[33m✎\033[0m %s: target modified\n", dst)
				modified++
				continue
			}

			// Mismatch but no lockfile entry
			fmt.Printf("  \033[33m≠\033[0m %s: differs from source\n", dst)
			modified++
		}
	}

	// Check for orphaned lockfile entries
	orphaned := 0
	for dstAbs, locked := range lf.Files {
		if !seen[dstAbs] {
			home, _ := os.UserHomeDir()
			display := strings.Replace(dstAbs, home, "~", 1)
			fmt.Printf("  \033[90m⊘\033[0m %s: orphaned (was from %s)\n", display, locked.Module)
			orphaned++
		}
	}

	total := current + modified + missing + sourceChanged + orphaned + symlinks
	fmt.Printf("\n%d files: %d current, %d source-updated, %d target-modified, %d missing, %d symlinks, %d orphaned\n",
		total, current, sourceChanged, modified, missing, symlinks, orphaned)
	return nil
}

func cmdDiff(dotfilesDir string, target string) error {
	lf := loadLockfile()
	targetAbs := expandHome(target)

	locked, ok := lf.Files[targetAbs]
	if !ok {
		return fmt.Errorf("%s: not in lockfile", target)
	}

	if _, err := os.Stat(locked.Source); err != nil {
		return fmt.Errorf("source not found: %s", locked.Source)
	}
	if _, err := os.Stat(targetAbs); err != nil {
		return fmt.Errorf("target not found: %s", targetAbs)
	}

	cmd := exec.Command("diff", "-u", locked.Source, targetAbs)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run() // exit code 1 means differences exist, that's fine
	return nil
}

func cmdClean() error {
	lf := loadLockfile()
	cleaned := 0

	for dstAbs, locked := range lf.Files {
		// Remove orphaned entries where source no longer exists
		if _, err := os.Stat(locked.Source); err != nil {
			home, _ := os.UserHomeDir()
			display := strings.Replace(dstAbs, home, "~", 1)
			fmt.Printf("  \033[33m⊘\033[0m %s: removing orphaned lockfile entry\n", display)
			delete(lf.Files, dstAbs)
			cleaned++
		}
	}

	if cleaned > 0 {
		if err := lf.Save(); err != nil {
			return err
		}
		fmt.Printf("\n%d orphaned entries removed\n", cleaned)
	} else {
		fmt.Println("no orphaned entries")
	}
	return nil
}

// ── Main ───────────────────────────────────────────────────────────

func resolveDotfilesDir() string {
	// Explicit flag
	if dir := os.Getenv("PUNCH_DOTFILES"); dir != "" {
		return dir
	}

	// Walk up from cwd looking for a punch dotfiles root
	// (has at least one dot.yaml somewhere)
	cwd, _ := os.Getwd()
	dir := cwd
	for dir != "/" {
		if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}

	// Default
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "ghq", "github.com", "nodeselector", "ns-dotfiles")
}

func newShellCmd(cmd string) *exec.Cmd {
	proc := exec.Command("bash", "-c", cmd)
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	proc.Stdin = os.Stdin
	return proc
}

func usage() {
	fmt.Fprintf(os.Stderr, `punch -- copy-based dotfile manager with provenance tracking

Usage: punch [flags] <command>

Commands:
  link [--force] [--dry-run]   Copy dotfiles to their targets
  install                      Run install commands (respects depends)
  status                       Show drift between source and targets
  diff <target>                Show diff between source and installed target
  clean                        Remove orphaned lockfile entries
  deps lock [name...]          Resolve deps to latest, write deps.lock.yaml
  deps install                 Fetch deps per lockfile, verify digests
  deps verify                  Check installed deps match lockfile
  deps update [name...]        Alias for deps lock (re-resolve to latest)
  deps list                    Show deps and their lock status

Flags:
  --dotfiles <path>   Dotfiles directory (default: auto-detect from cwd)

Environment:
  PUNCH_DOTFILES       Override dotfiles directory
  GITHUB_TOKEN/GH_TOKEN  GitHub API token (for deps commands)
`)
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	var dotfilesDir string
	var force, dryRun bool

	// Parse flags
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dotfiles":
			if i+1 < len(args) {
				dotfilesDir = args[i+1]
				i++
			}
		case "--force", "-f":
			force = true
		case "--dry-run", "-n":
			dryRun = true
		case "--help", "-h":
			usage()
			os.Exit(0)
		default:
			positional = append(positional, args[i])
		}
	}

	if dotfilesDir == "" {
		dotfilesDir = resolveDotfilesDir()
	}

	if len(positional) == 0 {
		usage()
		os.Exit(1)
	}

	cmd := positional[0]

	// Sort modules consistently
	_ = sort.Strings

	var err error
	switch cmd {
	case "link":
		err = cmdLink(dotfilesDir, force, dryRun)
	case "install":
		err = cmdInstall(dotfilesDir)
	case "status":
		err = cmdStatus(dotfilesDir)
	case "diff":
		if len(positional) < 2 {
			fmt.Fprintln(os.Stderr, "usage: punch diff <target>")
			os.Exit(1)
		}
		err = cmdDiff(dotfilesDir, positional[1])
	case "deps":
		if len(positional) < 2 {
			fmt.Fprintln(os.Stderr, "usage: punch deps <lock|install|verify|update|list> [name...]")
			os.Exit(1)
		}
		subcmd := positional[1]
		names := positional[2:]
		switch subcmd {
		case "lock", "update":
			err = cmdDepsLock(dotfilesDir, names)
		case "install":
			err = cmdDepsInstall(dotfilesDir)
		case "verify":
			err = cmdDepsVerify(dotfilesDir)
		case "list", "ls":
			err = cmdDepsList(dotfilesDir)
		default:
			fmt.Fprintf(os.Stderr, "unknown deps subcommand: %s\n", subcmd)
			os.Exit(1)
		}
	case "clean":
		err = cmdClean()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
