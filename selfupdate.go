package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// defaultReleasesRepo is a placeholder — override at build time with
// -ldflags "-X main.defaultReleasesRepo=owner/repo" (same mechanism as
// defaultAPIBase in main.go), or per-invocation with --repo /
// RUNGUARD_CLI_REPO. Self-update pulls releases straight from GitHub's
// public Releases API — no custom versions API or binary host needed.
var defaultReleasesRepo = "your-org/your-cli-repo"

type githubRelease struct {
	TagName string               `json:"tag_name"`
	Assets  []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// SelfUpdate implements: runguard self-update [--version=vX.Y.Z] [--repo=owner/repo]
func SelfUpdate(args []string) error {
	fs := flag.NewFlagSet("self-update", flag.ExitOnError)
	versionFlag := fs.String("version", "", "Install this specific release tag (e.g. v0.1.3) instead of the latest")
	repoFlag := fs.String("repo", envOr("RUNGUARD_CLI_REPO", defaultReleasesRepo), "GitHub owner/repo to fetch releases from (or RUNGUARD_CLI_REPO env var)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: runguard self-update [flags]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	release, err := fetchRelease(*repoFlag, strings.TrimSpace(*versionFlag))
	if err != nil {
		return fmt.Errorf("fetch release: %w", err)
	}

	if Version != "" && (Version == release.TagName || "v"+Version == release.TagName) {
		fmt.Printf("runguard is already at version %s\n", release.TagName)
		return nil
	}

	assetName := fmt.Sprintf("runguard-%s-%s", runtime.GOOS, runtime.GOARCH)
	binURL, sha256URL, err := findAsset(release, assetName)
	if err != nil {
		return err
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	tmpPath := exePath + ".update-tmp"
	defer os.Remove(tmpPath)

	fmt.Printf("Downloading %s (version %s)...\n", assetName, release.TagName)
	if err := httpDownload(binURL, tmpPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	fmt.Println("Verifying checksum...")
	expectedHex, err := fetchSHA256(sha256URL)
	if err != nil {
		return fmt.Errorf("fetch checksum: %w", err)
	}
	if err := verifySHA256(tmpPath, expectedHex); err != nil {
		return fmt.Errorf("checksum verification failed: %w", err)
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("chmod new binary: %w", err)
	}

	if err := os.Rename(tmpPath, exePath); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("permission denied replacing %s -- try running with sudo: %w", exePath, err)
		}
		return fmt.Errorf("replace binary: %w", err)
	}

	fmt.Printf("Updated to version %s.\n", release.TagName)
	return nil
}

// fetchRelease hits GitHub's public Releases API directly — no auth, no
// custom backend involved. version == "" fetches the latest release;
// otherwise fetches that exact tag.
func fetchRelease(repo, version string) (*githubRelease, error) {
	repo = strings.Trim(repo, "/")
	if repo == "" {
		return nil, fmt.Errorf("--repo or RUNGUARD_CLI_REPO is required (e.g. smega/runguard-cli)")
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	if version != "" {
		url = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, version)
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("GitHub API returned %s for %s: %s", resp.Status, url, strings.TrimSpace(string(body)))
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode GitHub release response: %w", err)
	}
	return &rel, nil
}

// findAsset locates the binary and its .sha256 sibling among a release's
// assets, by the exact name release.yml publishes them under
// (runguard-{goos}-{goarch}, no version suffix — the tag carries that).
func findAsset(release *githubRelease, name string) (binaryURL, sha256URL string, err error) {
	for _, a := range release.Assets {
		switch a.Name {
		case name:
			binaryURL = a.BrowserDownloadURL
		case name + ".sha256":
			sha256URL = a.BrowserDownloadURL
		}
	}
	if binaryURL == "" {
		return "", "", fmt.Errorf("release %s has no asset named %q", release.TagName, name)
	}
	if sha256URL == "" {
		return "", "", fmt.Errorf("refusing to install: no %s.sha256 asset found for release %s", name, release.TagName)
	}
	return binaryURL, sha256URL, nil
}

func fetchSHA256(sha256URL string) (string, error) {
	resp, err := http.Get(sha256URL) // #nosec G107
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("sha256 URL returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty sha256 file")
	}
	return strings.TrimSpace(fields[0]), nil
}

func httpDownload(srcURL, destPath string) error {
	resp, err := http.Get(srcURL) // #nosec G107
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download returned %s", resp.Status)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func verifySHA256(path, expectedHex string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))

	if !strings.EqualFold(got, strings.TrimSpace(expectedHex)) {
		return fmt.Errorf("checksum mismatch: got %s, expected %s", got, expectedHex)
	}
	return nil
}
