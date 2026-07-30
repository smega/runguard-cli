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
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	defaultVersionsAPI  = "https://runguard.smega.eu/api/public/cli/versions"
	defaultChannel      = "stable"
	defaultDownloadBase = "https://runguard-download.smega.eu"
)

// versionsAPIResponse is the shape of defaultVersionsAPI's JSON response.
// Kept intentionally minimal: version string, channel, and a per-platform
// map of binary filenames plus their sha256 checksums.
type versionsAPIResponse struct {
	Version   string            `json:"version"`
	Channel   string            `json:"channel"`
	Binaries  map[string]string `json:"binaries"`  // "linux/amd64" -> filename
	Checksums map[string]string `json:"checksums"` // filename -> sha256 hex
}

// SelfUpdate implements: runguard self-update [--api=URL] [--channel=stable] [--yes]
func SelfUpdate(args []string) error {
	fs := flag.NewFlagSet("self-update", flag.ExitOnError)
	api := fs.String("api", defaultVersionsAPI, "Versions API URL")
	channel := fs.String("channel", defaultChannel, "Release channel")
	downloadBase := fs.String("download-base", defaultDownloadBase, "Base URL to download binaries from")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: runguard self-update [flags]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	info, err := fetchVersions(*api, *channel)
	if err != nil {
		return fmt.Errorf("fetch versions: %w", err)
	}

	platform := runtime.GOOS + "/" + runtime.GOARCH
	filename, ok := info.Binaries[platform]
	if !ok {
		return fmt.Errorf("no published binary for platform %s", platform)
	}
	checksum, ok := info.Checksums[filename]
	if !ok || strings.TrimSpace(checksum) == "" {
		return fmt.Errorf("no checksum published for %s -- refusing to install an unverified binary", filename)
	}

	binURL, err := composeBinaryURL(*downloadBase, info.Channel, filename)
	if err != nil {
		return fmt.Errorf("build download URL: %w", err)
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

	fmt.Printf("Downloading %s (version %s)...\n", filename, info.Version)
	if err := httpDownload(binURL, tmpPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	fmt.Println("Verifying checksum...")
	if err := verifySHA256(tmpPath, checksum); err != nil {
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

	fmt.Printf("Updated to version %s.\n", info.Version)
	return nil
}

func fetchVersions(apiURL, channel string) (*versionsAPIResponse, error) {
	u, err := url.Parse(apiURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("channel", channel)
	q.Set("os", runtime.GOOS)
	q.Set("arch", runtime.GOARCH)
	u.RawQuery = q.Encode()

	resp, err := http.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("versions API returned %s", resp.Status)
	}

	var out versionsAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode versions response: %w", err)
	}
	return &out, nil
}

func composeBinaryURL(downloadBase, channel, filename string) (string, error) {
	base := strings.TrimRight(downloadBase, "/")
	full := fmt.Sprintf("%s/%s/%s", base, channel, filename)
	if _, err := url.Parse(full); err != nil {
		return "", err
	}
	return full, nil
}

func httpDownload(srcURL, destPath string) error {
	resp, err := http.Get(srcURL)
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
