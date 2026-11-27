package cmd

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// githubRepo is this fork, not upstream: "weclaw upgrade" must never pull an
// official binary over the customized build.
const githubRepo = "KonoLLo/weclaw"

func init() {
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(upgradeCmd)
	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the current version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("weclaw %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
	},
}

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update weclaw to the latest version and restart",
	RunE:  runUpdate,
}

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Update weclaw to the latest version and restart (alias for update)",
	RunE:  runUpdate,
}

func runUpdate(cmd *cobra.Command, args []string) error {
	// 1. Get latest version
	fmt.Println("Checking for updates...")
	latest, err := getLatestVersion()
	if err != nil {
		return fmt.Errorf("failed to check latest version: %w", err)
	}

	if cmp, ok := compareVersions(Version, latest); ok {
		if cmp == 0 {
			fmt.Printf("Already up to date (%s)\n", Version)
			return nil
		}
		if cmp > 0 {
			fmt.Printf("Local build %s is newer than latest release %s, nothing to do\n", Version, latest)
			return nil
		}
	} else if latest == Version {
		fmt.Printf("Already up to date (%s)\n", Version)
		return nil
	}

	fmt.Printf("Current: %s -> Latest: %s (github.com/%s)\n", Version, latest, githubRepo)

	// 2. Download new binary
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	filename := fmt.Sprintf("weclaw_%s_%s", goos, goarch)
	if goos == "windows" {
		filename += ".exe"
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", githubRepo, latest, filename)

	fmt.Printf("Downloading %s...\n", url)
	tmpFile, err := downloadFile(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer os.Remove(tmpFile)

	// 3. Replace current binary
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	// Resolve symlinks
	if resolved, err := resolveSymlink(exePath); err == nil {
		exePath = resolved
	}

	if err := replaceBinary(tmpFile, exePath); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}

	// Clear macOS quarantine/provenance attributes to avoid Gatekeeper killing the binary
	if runtime.GOOS == "darwin" {
		exec.Command("xattr", "-d", "com.apple.quarantine", exePath).Run()
		exec.Command("xattr", "-d", "com.apple.provenance", exePath).Run()
	}

	fmt.Printf("Updated to %s\n", latest)

	// 4. Restart if running in background
	pid, pidErr := readPid()
	if pidErr == nil && processExists(pid) {
		fmt.Println("Stopping old process...")
		if p, err := os.FindProcess(pid); err == nil {
			p.Signal(os.Interrupt)
		}
		// Wait for old process to exit
		for i := 0; i < 20; i++ {
			if !processExists(pid) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		os.Remove(pidFile())

		fmt.Println("Starting new version...")
		if err := runDaemon(); err != nil {
			log.Printf("Failed to restart: %v", err)
			fmt.Println("Update complete. Please run 'weclaw start' manually.")
		}
	} else {
		if runtime.GOOS == "linux" && restartSystemdService() {
			fmt.Println("Restarted weclaw.service")
		} else {
			fmt.Println("Update complete. Run 'weclaw start' to start.")
		}
	}

	return nil
}

// compareVersions compares dotted numeric versions with an optional leading
// "v". It reports false when either side is not numeric, so callers can fall
// back to plain string comparison.
func compareVersions(a, b string) (int, bool) {
	parse := func(s string) ([3]int, bool) {
		s = strings.TrimPrefix(strings.TrimSpace(s), "v")
		var out [3]int
		parts := strings.Split(s, ".")
		if len(parts) == 0 || len(parts) > 3 {
			return out, false
		}
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				return out, false
			}
			out[i] = n
		}
		return out, true
	}
	av, ok := parse(a)
	if !ok {
		return 0, false
	}
	bv, ok := parse(b)
	if !ok {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// restartSystemdService restarts the weclaw unit when this binary was started
// by systemd. Failure is non-fatal: the new binary is already in place.
func restartSystemdService() bool {
	out, err := exec.Command("systemctl", "is-active", "weclaw").Output()
	if err != nil || strings.TrimSpace(string(out)) != "active" {
		return false
	}
	fmt.Println("Restarting weclaw service...")
	cmd := exec.Command("systemctl", "restart", "weclaw")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run() == nil
}

func getLatestVersion() (string, error) {
	// Use HTTP redirect instead of API to avoid rate limits
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(fmt.Sprintf("https://github.com/%s/releases/latest", githubRepo))
	if err != nil {
		return "", err
	}
	resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("no redirect from GitHub releases/latest")
	}
	parts := strings.Split(loc, "/tag/")
	if len(parts) != 2 || parts[1] == "" {
		return "", fmt.Errorf("unexpected redirect URL: %s", loc)
	}
	return parts[1], nil
}

func downloadFile(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "weclaw-update-*")
	if err != nil {
		return "", err
	}

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	tmp.Close()

	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}

	return tmp.Name(), nil
}

func replaceBinary(src, dst string) error {
	// Check if we can write directly
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	if runtime.GOOS == "windows" {
		// On Windows the running binary is locked. Move it aside first,
		// then place the new binary. The old file will be cleaned up on
		// next restart or can be deleted manually.
		old := dst + ".old"
		os.Remove(old)
		if err := os.Rename(dst, old); err != nil {
			return fmt.Errorf("cannot move old binary aside: %w", err)
		}
		if err := os.Rename(src, dst); err != nil {
			os.Rename(old, dst)
			return fmt.Errorf("cannot install new binary: %w", err)
		}
		return nil
	}

	// Try with sudo on Unix
	fmt.Printf("Installing to %s (requires sudo)...\n", dst)
	cmd := exec.Command("sudo", "cp", src, dst)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func resolveSymlink(path string) (string, error) {
	for {
		target, err := os.Readlink(path)
		if err != nil {
			return path, nil
		}
		if !strings.HasPrefix(target, "/") {
			// Relative symlink
			dir := path[:strings.LastIndex(path, "/")+1]
			target = dir + target
		}
		path = target
	}
}
