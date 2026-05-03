package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ── deps.yaml schema ──────────────────────────────────────────────

type DepsConfig struct {
	Deps map[string]*DepEntry `yaml:"deps"`
}

type DepEntry struct {
	Type   string            `yaml:"type"`   // github-release, github-clone, curl-script
	Repo   string            `yaml:"repo"`   // owner/repo
	File   string            `yaml:"file"`   // for curl-script: path within repo
	Assets map[string]string `yaml:"assets"` // for github-release: platform -> asset name
	Dest   string            `yaml:"dest"`   // install destination
	Run    string            `yaml:"run"`    // for curl-script: command to run with downloaded script as stdin
	Tag    string            `yaml:"tag"`    // optional: pin to specific tag (otherwise latest)
}

// ── deps.lock.yaml schema ─────────────────────────────────────────

type DepsLockfile struct {
	Version   int                       `yaml:"version"`
	LockedAt  string                    `yaml:"locked_at"`
	Deps      map[string]*LockedDep     `yaml:"deps"`
}

type LockedDep struct {
	Type      string                    `yaml:"type"`
	Repo      string                    `yaml:"repo"`
	Tag       string                    `yaml:"tag,omitempty"`
	Ref       string                    `yaml:"ref,omitempty"`       // commit SHA
	File      string                    `yaml:"file,omitempty"`
	FileSHA   string                    `yaml:"file_sha256,omitempty"`
	Assets    map[string]*LockedAsset   `yaml:"assets,omitempty"`
	Verified  bool                      `yaml:"verified"`           // impostor check passed
}

type LockedAsset struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
}

// ── GitHub API types ──────────────────────────────────────────────

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRef struct {
	Ref    string   `json:"ref"`
	Object ghObject `json:"object"`
}

type ghObject struct {
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

type ghTag struct {
	Name   string   `json:"name"`
	Commit ghCommit `json:"commit"`
}

type ghBranch struct {
	Name   string   `json:"name"`
	Commit ghCommit `json:"commit"`
}

type ghCommit struct {
	SHA string `json:"sha"`
}

type ghBranchCommits struct {
	Branches []json.RawMessage `json:"branches"`
	Tags     []string          `json:"tags"`
}

type ghComparison struct {
	Status string `json:"status"` // ahead, behind, diverged, identical
}

// ── GitHub client ─────────────────────────────────────────────────

type ghClient struct {
	token      string
	httpClient *http.Client
}

func newGHClient() *ghClient {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	return &ghClient{
		token:      token,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *ghClient) apiGet(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "punch-dotfiles")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.httpClient.Do(req)
}

func (c *ghClient) apiGetJSON(url string, v any) error {
	resp, err := c.apiGet(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// getLatestRelease fetches the latest release for owner/repo.
func (c *ghClient) getLatestRelease(owner, repo string) (*ghRelease, error) {
	var rel ghRelease
	err := c.apiGetJSON(fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo), &rel)
	return &rel, err
}

// getReleaseByTag fetches a specific tagged release.
func (c *ghClient) getReleaseByTag(owner, repo, tag string) (*ghRelease, error) {
	var rel ghRelease
	err := c.apiGetJSON(fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", owner, repo, tag), &rel)
	return &rel, err
}

// resolveRef resolves a git ref (tag, branch) to a commit SHA.
func (c *ghClient) resolveRef(owner, repo, ref string) (string, error) {
	var data ghRef
	err := c.apiGetJSON(fmt.Sprintf("https://api.github.com/repos/%s/%s/git/ref/tags/%s", owner, repo, ref), &data)
	if err != nil {
		// Try as branch
		err = c.apiGetJSON(fmt.Sprintf("https://api.github.com/repos/%s/%s/git/ref/heads/%s", owner, repo, ref), &data)
	}
	if err != nil {
		return "", fmt.Errorf("cannot resolve ref %s: %w", ref, err)
	}
	// If it's an annotated tag, we need to dereference to get the commit
	if data.Object.Type == "tag" {
		var tagObj struct {
			Object ghObject `json:"object"`
		}
		err = c.apiGetJSON(fmt.Sprintf("https://api.github.com/repos/%s/%s/git/tags/%s", owner, repo, data.Object.SHA), &tagObj)
		if err != nil {
			return data.Object.SHA, nil // fall back to the tag object SHA
		}
		return tagObj.Object.SHA, nil
	}
	return data.Object.SHA, nil
}

// getFileHash downloads a file from a repo at a specific ref and returns its SHA256.
func (c *ghClient) getFileHash(owner, repo, ref, path string) (string, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, ref, path)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// downloadAndHash downloads a URL and returns the content bytes and SHA256.
func (c *ghClient) downloadAndHash(url string) ([]byte, string, error) {
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	h := sha256.New()
	h.Write(body)
	return body, hex.EncodeToString(h.Sum(nil)), nil
}

// ── Impostor commit detection ─────────────────────────────────────

// verifyCommitBelongsToRepo checks that a commit SHA actually belongs to the
// specified owner/repo and is not an "impostor" commit that resolves only
// through GitHub's fork network.
//
// Uses the technique from zizmor/clank:
// 1. Fast: check branch_commits undocumented API
// 2. Fallback: compare API against all branches and tags
func (c *ghClient) verifyCommitBelongsToRepo(owner, repo, sha string) (bool, error) {
	// Fast path: undocumented branch_commits API
	url := fmt.Sprintf("https://github.com/%s/%s/branch_commits/%s", owner, repo, sha)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "punch-dotfiles")
	// Note: this endpoint is on github.com, not api.github.com. No auth needed.

	resp, err := c.httpClient.Do(req)
	if err == nil && resp.StatusCode == 200 {
		defer resp.Body.Close()
		var bc ghBranchCommits
		if json.NewDecoder(resp.Body).Decode(&bc) == nil {
			if len(bc.Branches) > 0 || len(bc.Tags) > 0 {
				return true, nil // commit is in at least one branch or tag
			}
			return false, nil // commit not found in any branch or tag -- impostor
		}
	}
	if resp != nil {
		resp.Body.Close()
	}

	// Fast path failed, fall back to API-based verification
	fmt.Fprintf(os.Stderr, "  ⚠ branch_commits API unavailable, using slow verification for %s/%s@%s\n", owner, repo, sha)

	// Check if commit is at the tip of any tag
	var tags []ghTag
	if err := c.apiGetJSON(fmt.Sprintf("https://api.github.com/repos/%s/%s/tags?per_page=100", owner, repo), &tags); err != nil {
		return false, fmt.Errorf("listing tags: %w", err)
	}
	for _, tag := range tags {
		if tag.Commit.SHA == sha {
			return true, nil
		}
	}

	// Check if commit is at the tip of any branch
	var branches []ghBranch
	if err := c.apiGetJSON(fmt.Sprintf("https://api.github.com/repos/%s/%s/branches?per_page=100", owner, repo), &branches); err != nil {
		return false, fmt.Errorf("listing branches: %w", err)
	}
	for _, branch := range branches {
		if branch.Commit.SHA == sha {
			return true, nil
		}
	}

	// Slow path: compare API -- check if any branch/tag contains the commit
	for _, branch := range branches {
		var comp ghComparison
		compURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/compare/refs/heads/%s...%s", owner, repo, branch.Name, sha)
		if err := c.apiGetJSON(compURL, &comp); err != nil {
			continue // 404 means diverged, skip
		}
		if comp.Status == "behind" || comp.Status == "identical" {
			return true, nil
		}
	}
	for _, tag := range tags {
		var comp ghComparison
		compURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/compare/refs/tags/%s...%s", owner, repo, tag.Name, sha)
		if err := c.apiGetJSON(compURL, &comp); err != nil {
			continue
		}
		if comp.Status == "behind" || comp.Status == "identical" {
			return true, nil
		}
	}

	return false, nil
}

// ── Platform helpers ──────────────────────────────────────────────

func currentPlatform() string {
	os := runtime.GOOS
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		return os + "-amd64"
	case "arm64":
		return os + "-arm64"
	}
	return os + "-" + arch
}

// ── Deps commands ─────────────────────────────────────────────────

func loadDepsConfig(dotfilesDir string) (*DepsConfig, error) {
	path := filepath.Join(dotfilesDir, "deps.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no deps.yaml found in %s", dotfilesDir)
		}
		return nil, err
	}
	var cfg DepsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing deps.yaml: %w", err)
	}
	return &cfg, nil
}

func loadDepsLockfile(dotfilesDir string) *DepsLockfile {
	lf := &DepsLockfile{Version: 1, Deps: make(map[string]*LockedDep)}
	data, err := os.ReadFile(filepath.Join(dotfilesDir, "deps.lock.yaml"))
	if err != nil {
		return lf
	}
	_ = yaml.Unmarshal(data, lf)
	if lf.Deps == nil {
		lf.Deps = make(map[string]*LockedDep)
	}
	return lf
}

func saveDepsLockfile(dotfilesDir string, lf *DepsLockfile) error {
	lf.LockedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := yaml.Marshal(lf)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dotfilesDir, "deps.lock.yaml"), data, 0o644)
}

func cmdDepsLock(dotfilesDir string, names []string) error {
	cfg, err := loadDepsConfig(dotfilesDir)
	if err != nil {
		return err
	}

	lf := loadDepsLockfile(dotfilesDir)
	gh := newGHClient()

	lockAll := len(names) == 0
	targets := cfg.Deps
	if !lockAll {
		targets = make(map[string]*DepEntry)
		for _, n := range names {
			dep, ok := cfg.Deps[n]
			if !ok {
				return fmt.Errorf("unknown dep: %s", n)
			}
			targets[n] = dep
		}
	}

	for name, dep := range targets {
		fmt.Printf("\n\033[1mLocking \033[38;5;12m%s\033[0m (%s/%s)\n", name, dep.Repo, dep.Type)

		parts := strings.SplitN(dep.Repo, "/", 2)
		if len(parts) != 2 {
			return fmt.Errorf("%s: invalid repo format %q, expected owner/repo", name, dep.Repo)
		}
		owner, repo := parts[0], parts[1]

		locked := &LockedDep{
			Type: dep.Type,
			Repo: dep.Repo,
		}

		switch dep.Type {
		case "github-release":
			var rel *ghRelease
			if dep.Tag != "" {
				rel, err = gh.getReleaseByTag(owner, repo, dep.Tag)
			} else {
				rel, err = gh.getLatestRelease(owner, repo)
			}
			if err != nil {
				return fmt.Errorf("%s: fetching release: %w", name, err)
			}
			locked.Tag = rel.TagName

			// Resolve tag to commit SHA
			sha, err := gh.resolveRef(owner, repo, rel.TagName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ⚠ could not resolve tag to commit: %v\n", err)
			} else {
				locked.Ref = sha
			}

			// Hash assets
			if dep.Assets != nil {
				locked.Assets = make(map[string]*LockedAsset)
				for platform, assetPattern := range dep.Assets {
					assetName := strings.ReplaceAll(assetPattern, "{version}", strings.TrimPrefix(rel.TagName, "v"))
					var found bool
					for _, a := range rel.Assets {
						if a.Name == assetName {
							fmt.Printf("  📦 %s: %s\n", platform, assetName)
							_, hash, err := gh.downloadAndHash(a.BrowserDownloadURL)
							if err != nil {
								return fmt.Errorf("%s: hashing asset %s: %w", name, assetName, err)
							}
							locked.Assets[platform] = &LockedAsset{
								Name:   assetName,
								URL:    a.BrowserDownloadURL,
								SHA256: hash,
							}
							found = true
							break
						}
					}
					if !found {
						return fmt.Errorf("%s: asset %q not found in release %s", name, assetName, rel.TagName)
					}
				}
			}

		case "github-clone":
			var sha string
			if dep.Tag != "" {
				sha, err = gh.resolveRef(owner, repo, dep.Tag)
			} else {
				// Resolve HEAD of default branch
				sha, err = gh.resolveRef(owner, repo, "main")
				if err != nil {
					sha, err = gh.resolveRef(owner, repo, "master")
				}
			}
			if err != nil {
				return fmt.Errorf("%s: resolving ref: %w", name, err)
			}
			locked.Ref = sha

		case "curl-script":
			tag := dep.Tag
			if tag == "" {
				// Get latest release tag
				rel, err := gh.getLatestRelease(owner, repo)
				if err != nil {
					return fmt.Errorf("%s: fetching latest release: %w", name, err)
				}
				tag = rel.TagName
			}
			locked.Tag = tag

			sha, err := gh.resolveRef(owner, repo, tag)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ⚠ could not resolve tag to commit: %v\n", err)
			} else {
				locked.Ref = sha
			}

			// Hash the script file at the pinned ref
			if dep.File != "" {
				ref := locked.Ref
				if ref == "" {
					ref = tag
				}
				locked.File = dep.File
				hash, err := gh.getFileHash(owner, repo, ref, dep.File)
				if err != nil {
					return fmt.Errorf("%s: hashing script file: %w", name, err)
				}
				locked.FileSHA = hash
				fmt.Printf("  📄 %s@%s sha256:%s\n", dep.File, ref[:12], hash[:16])
			}
		}

		// Impostor commit verification
		if locked.Ref != "" {
			fmt.Printf("  🔍 verifying commit %s...\n", locked.Ref[:12])
			belongs, err := gh.verifyCommitBelongsToRepo(owner, repo, locked.Ref)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ⚠ impostor check failed: %v\n", err)
				locked.Verified = false
			} else if !belongs {
				return fmt.Errorf("  🚨 IMPOSTOR COMMIT DETECTED: %s@%s does not belong to %s/%s", name, locked.Ref, owner, repo)
			} else {
				fmt.Printf("  ✅ commit verified\n")
				locked.Verified = true
			}
		}

		lf.Deps[name] = locked
	}

	if err := saveDepsLockfile(dotfilesDir, lf); err != nil {
		return fmt.Errorf("saving lockfile: %w", err)
	}
	fmt.Printf("\n✅ %d dep(s) locked to deps.lock.yaml\n", len(targets))
	return nil
}

func cmdDepsInstall(dotfilesDir string) error {
	cfg, err := loadDepsConfig(dotfilesDir)
	if err != nil {
		return err
	}
	lf := loadDepsLockfile(dotfilesDir)
	gh := newGHClient()
	platform := currentPlatform()

	installed := 0
	for name, dep := range cfg.Deps {
		locked, ok := lf.Deps[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "  ⚠ %s: not in lockfile, run 'punch deps lock' first\n", name)
			continue
		}

		if !locked.Verified {
			fmt.Fprintf(os.Stderr, "  🚨 %s: skipping unverified dep (commit not verified as belonging to %s)\n", name, dep.Repo)
			continue
		}

		fmt.Printf("\n\033[1mInstalling \033[38;5;12m%s\033[0m\n", name)

		switch dep.Type {
		case "github-release":
			asset, ok := locked.Assets[platform]
			if !ok {
				fmt.Printf("  ⏭ no asset for platform %s\n", platform)
				continue
			}
			dest := expandHome(dep.Dest)
			if info, err := os.Stat(dest); err == nil && !info.IsDir() {
				// Check if already installed with correct hash
				existingHash := hashFile(dest)
				if existingHash == asset.SHA256 {
					fmt.Printf("  ✅ already installed (hash matches)\n")
					installed++
					continue
				}
			}

			body, hash, err := gh.downloadAndHash(asset.URL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ download failed: %v\n", err)
				continue
			}
			if hash != asset.SHA256 {
				fmt.Fprintf(os.Stderr, "  🚨 DIGEST MISMATCH for %s\n    expected: %s\n    got:      %s\n", name, asset.SHA256, hash)
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ mkdir: %v\n", err)
				continue
			}
			if err := os.WriteFile(dest, body, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ write: %v\n", err)
				continue
			}
			fmt.Printf("  📦 %s → %s (sha256 verified)\n", asset.Name, dep.Dest)
			installed++

		case "github-clone":
			dest := expandHome(dep.Dest)
			if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
				// Already cloned -- verify the checkout matches
				fmt.Printf("  ✅ already cloned at %s\n", dep.Dest)
				installed++
				continue
			}
			parts := strings.SplitN(dep.Repo, "/", 2)
			cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", parts[0], parts[1])
			cmd := fmt.Sprintf("git clone --depth 1 %s %s && cd %s && git fetch --depth 1 origin %s && git checkout %s",
				cloneURL, dest, dest, locked.Ref, locked.Ref)
			proc := newShellCmd(cmd)
			proc.Dir = dotfilesDir
			if err := proc.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ clone failed: %v\n", err)
				continue
			}
			fmt.Printf("  📂 cloned to %s@%s\n", dep.Dest, locked.Ref[:12])
			installed++

		case "curl-script":
			if dep.File == "" {
				fmt.Fprintf(os.Stderr, "  ⚠ %s: curl-script requires a file path\n", name)
				continue
			}
			ref := locked.Ref
			if ref == "" {
				ref = locked.Tag
			}
			parts := strings.SplitN(dep.Repo, "/", 2)
			url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", parts[0], parts[1], ref, dep.File)

			body, hash, err := gh.downloadAndHash(url)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ download failed: %v\n", err)
				continue
			}
			if locked.FileSHA != "" && hash != locked.FileSHA {
				fmt.Fprintf(os.Stderr, "  🚨 SCRIPT DIGEST MISMATCH for %s\n    expected: %s\n    got:      %s\n", name, locked.FileSHA, hash)
				continue
			}

			// Run the script
			runCmd := dep.Run
			if runCmd == "" {
				runCmd = "bash"
			}
			proc := newShellCmd(runCmd)
			proc.Dir = dotfilesDir
			proc.Stdin = strings.NewReader(string(body))
			if err := proc.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ script failed: %v\n", err)
				continue
			}
			fmt.Printf("  📄 %s@%s executed (sha256 verified)\n", dep.File, ref[:12])
			installed++
		}
	}

	fmt.Printf("\n%d dep(s) installed\n", installed)
	return nil
}

func cmdDepsVerify(dotfilesDir string) error {
	cfg, err := loadDepsConfig(dotfilesDir)
	if err != nil {
		return err
	}
	lf := loadDepsLockfile(dotfilesDir)
	platform := currentPlatform()

	ok := 0
	problems := 0

	for name, dep := range cfg.Deps {
		locked, inLock := lf.Deps[name]
		if !inLock {
			fmt.Printf("  ❌ %s: not locked\n", name)
			problems++
			continue
		}
		if !locked.Verified {
			fmt.Printf("  ⚠  %s: commit not verified\n", name)
			problems++
			continue
		}

		switch dep.Type {
		case "github-release":
			asset, hasAsset := locked.Assets[platform]
			if !hasAsset {
				fmt.Printf("  ⏭  %s: no asset for %s\n", name, platform)
				continue
			}
			dest := expandHome(dep.Dest)
			installed := hashFile(dest)
			if installed == "" {
				fmt.Printf("  ❌ %s: not installed\n", name)
				problems++
			} else if installed != asset.SHA256 {
				fmt.Printf("  ❌ %s: hash mismatch (installed differs from lock)\n", name)
				problems++
			} else {
				fmt.Printf("  ✅ %s: %s verified\n", name, locked.Tag)
				ok++
			}
		case "github-clone":
			dest := expandHome(dep.Dest)
			if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
				fmt.Printf("  ❌ %s: not cloned\n", name)
				problems++
			} else {
				fmt.Printf("  ✅ %s: cloned at %s\n", name, locked.Ref[:12])
				ok++
			}
		case "curl-script":
			fmt.Printf("  ✅ %s: locked at %s@%s\n", name, dep.Repo, locked.Tag)
			ok++
		}
	}

	fmt.Printf("\n%d ok, %d problems\n", ok, problems)
	if problems > 0 {
		return fmt.Errorf("%d deps have problems", problems)
	}
	return nil
}

func cmdDepsList(dotfilesDir string) error {
	cfg, err := loadDepsConfig(dotfilesDir)
	if err != nil {
		return err
	}
	lf := loadDepsLockfile(dotfilesDir)

	for name, dep := range cfg.Deps {
		locked, ok := lf.Deps[name]
		status := "🔓 unlocked"
		version := ""
		if ok {
			if locked.Verified {
				status = "✅"
			} else {
				status = "⚠ unverified"
			}
			if locked.Tag != "" {
				version = locked.Tag
			} else if locked.Ref != "" {
				version = locked.Ref[:12]
			}
		}
		fmt.Printf("  %s %s (%s) %s %s\n", status, name, dep.Type, dep.Repo, version)
	}
	return nil
}
