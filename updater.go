// relay_uploader.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	semver "github.com/Masterminds/semver/v3"
)

const (
	dlDir = "downloads"
)

type downloadInfo struct {
	Link     string `json:"link"`
	Checksum string `json:"sha256"`
}

type Entry struct {
	Version string         `json:"version"`
	Date    int64          `json:"utc-unixnano"`
	Links   []downloadInfo `json:"links"`
}

func main() {
	dryRun := flag.Bool("dry-run", false, "do not upload via ssh (testing)")
	srcDir := flag.String("src-dir", "../RelayClient", "directory to scan for .zip files")
	manualVer := flag.String("version", "", "manually specify new version (format a.b.c)")
	hostPort := flag.String("host", "host.ext", "SSH host[:port]")
	user := flag.String("user", "user", "SSH username")
	remoteDir := flag.String("remote-dir", "/home/user/www/public_html", "remote directory")
	jsonName := flag.String("json", "relayClient.json", "name of JSON file")
	flag.Parse()

	// ensure local release-dir exists
	if err := os.MkdirAll(dlDir, 0755); err != nil {
		fmt.Fprintln(os.Stderr, "failed to create release-dir:", err)
		os.Exit(1)
	}

	// load or initialize JSON
	entries, err := readEntries(*jsonName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to read JSON:", err)
		os.Exit(1)
	}

	// pick new version
	var newVersion string
	if *manualVer != "" {
		v, err := semver.NewVersion(*manualVer)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid -version %q: %v\n", *manualVer, err)
			os.Exit(1)
		}
		newVersion = v.String()
	} else {
		highest := semver.MustParse("0.0.0")
		for _, e := range entries {
			if v, err := semver.NewVersion(e.Version); err == nil && v.GreaterThan(highest) {
				highest = v
			}
		}
		newVersion = highest.IncPatch().String()
	}

	err = RunBuildAll(newVersion)
	if err != nil {
		log.Fatalf("Build process failed: %v", err)
	}

	// create version subfolder
	versionDir := filepath.Join(dlDir, newVersion)
	if err := os.MkdirAll(versionDir, 0755); err != nil {
		fmt.Fprintln(os.Stderr, "failed to create version dir:", err)
		os.Exit(1)
	}

	// copy & rename zips into releases/<version>/
	files, err := collectAndRenameZips(*srcDir, versionDir, newVersion)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error handling zip files:", err)
		os.Exit(1)
	}

	// build JSON entries using only filenames
	var links []downloadInfo
	for _, file := range files {

		fullPath := filepath.Join(versionDir, file)

		sum, err := computeChecksum(fullPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "checksum failed for %s: %v\n", fullPath, err)
			os.Exit(1)
		}

		links = append(links, downloadInfo{Link: fullPath, Checksum: sum})

	}

	// append entry & write JSON
	entries = upsertEntry(entries, Entry{
		Version: newVersion,
		Date:    time.Now().UTC().UnixNano(),
		Links:   links,
	})
	if err := writeEntries(*jsonName, entries); err != nil {
		fmt.Fprintln(os.Stderr, "failed to write JSON:", err)
		os.Exit(1)
	}

	// ensure remote version folder exists
	remoteVersionDir := strings.TrimRight(*remoteDir, "/") + "/" + dlDir + "/" + newVersion
	if err := ensureRemoteDir(*hostPort, *user, remoteVersionDir); err != nil {
		fmt.Fprintln(os.Stderr, "failed to mkdir on remote:", err)
		os.Exit(1)
	}

	// scp zips into remote/<version>/
	var localZips []string
	for _, f := range files {
		localZips = append(localZips, filepath.Join(versionDir, f))
	}
	if !*dryRun {

		if err := uploadWithScp(*hostPort, *user, remoteVersionDir, localZips...); err != nil {
			fmt.Fprintln(os.Stderr, "upload zips failed:", err)
			os.Exit(1)
		}

		if err := uploadWithScp(*hostPort, *user, *remoteDir, *jsonName); err != nil {
			fmt.Fprintln(os.Stderr, "upload JSON failed:", err)
			os.Exit(1)
		}

		if err := updateLatestFileSymlinks(*hostPort, *user, *remoteDir+"/"+dlDir, newVersion, files); err != nil {
			fmt.Fprintln(os.Stderr, "failed to update latest file‑symlinks:", err)
			os.Exit(1)
		}
	}

	fmt.Printf("✅ Released version %s in %s with %d file(s)\n",
		newVersion, versionDir, len(files))
}

func readEntries(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := writeEntries(path, []Entry{}); err != nil {
				return nil, err
			}
			return []Entry{}, nil
		}
		return nil, err
	}
	var ents []Entry
	if err := json.Unmarshal(data, &ents); err != nil {
		return nil, err
	}
	return ents, nil
}

func writeEntries(path string, ents []Entry) error {
	out, err := json.MarshalIndent(ents, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

func collectAndRenameZips(srcDir, versionDir, ver string) ([]string, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(de.Name()), ".zip") {
			base := strings.TrimSuffix(de.Name(), filepath.Ext(de.Name()))
			newName := fmt.Sprintf("%s-%s.zip", base, ver)
			if err := copyFile(filepath.Join(srcDir, de.Name()), filepath.Join(versionDir, newName)); err != nil {
				return nil, err
			}
			out = append(out, newName)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no .zip files found in " + srcDir)
	}
	return out, nil
}

func copyFile(src, dst string) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	fi, err := sf.Stat()
	if err != nil {
		return err
	}
	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode())
	if err != nil {
		return err
	}
	defer df.Close()
	_, err = io.Copy(df, sf)
	return err
}

func computeChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func ensureRemoteDir(hostPort, user, remotePath string) error {
	host, port := parseHostPort(hostPort)
	args := []string{}
	if port != "" {
		args = append(args, "-p", port)
	}
	args = append(args, fmt.Sprintf("%s@%s", user, host), "mkdir -p "+remotePath)
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func uploadWithScp(hostPort, user, remoteDir string, locals ...string) error {
	host, port := parseHostPort(hostPort)
	for _, local := range locals {
		args := []string{}
		if port != "" {
			args = append(args, "-P", port)
		}
		args = append(args, local, fmt.Sprintf("%s@%s:%s", user, host, remoteDir))
		cmd := exec.Command("scp", args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("scp %s failed: %w", local, err)
		}
	}
	return nil
}

func parseHostPort(hp string) (host, port string) {
	parts := strings.Split(hp, ":")
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return hp, ""
}

// updateLatestFileSymlinks SSH’s into the server and for each versioned file
// creates/updates a root‑level symlink pointing to the versioned path.
func updateLatestFileSymlinks(hostPort, user, remoteBase, newVersion string, files []string) error {
	host, port := parseHostPort(hostPort)

	for _, f := range files {
		// strip "-<version>.zip" → get "client.zip"
		generic := strings.TrimSuffix(f, "-"+newVersion+".zip") + "-latest.zip"
		target := filepath.Join(remoteBase, newVersion, f) // e.g. /.../0.2.5/client-0.2.5.zip
		link := filepath.Join(remoteBase, generic)         // e.g. /.../client.zip

		// build: ssh [-p port] user@host "ln -sfn <target> <link>"
		args := []string{}
		if port != "" {
			args = append(args, "-p", port)
		}
		args = append(args,
			fmt.Sprintf("%s@%s", user, host),
			fmt.Sprintf("ln -sfn %q %q", target, link),
		)
		cmd := exec.Command("ssh", args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("updating symlink for %s: %w", f, err)
		}
	}
	return nil
}

func upsertEntry(entries []Entry, newEntry Entry) []Entry {
	for i, e := range entries {
		if e.Version == newEntry.Version {
			entries[i] = newEntry
			return entries
		}
	}
	return append(entries, newEntry)
}

func RunBuildAll(version string) error {
	script := "../RelayClient/build/build-all.sh"

	// verify the script exists
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("cannot find script %q: %w", script, err)
	}

	// use bash to run the script and pass the version arg
	cmd := exec.Command("bash", script, version)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build-all.sh failed: %w", err)
	}
	return nil
}
