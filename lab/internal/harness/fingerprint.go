package harness

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Fingerprint binds approval to the local executable and provider configuration.
// It hashes configuration, never copies its credentials into the ledger. This is
// deliberately conservative: unrelated config edits may require refreshed intake.
func Fingerprint(command []string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("missing harness command")
	}
	files := []string{}
	environment := map[string]string{}
	addExecutable := func(name string) error {
		file, err := exec.LookPath(name)
		if err != nil {
			return err
		}
		files = append(files, file)
		return nil
	}
	if err := addExecutable(command[0]); err != nil {
		return "", err
	}
	if len(command) == 4 && command[1] == "__adapter" {
		if err := addExecutable(command[3]); err != nil {
			return "", err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		switch command[2] {
		case "codex":
			root := os.Getenv("CODEX_HOME")
			if root == "" {
				root = filepath.Join(home, ".codex")
			}
			files = append(files, filepath.Join(root, "config.toml"))
		case "claude-code":
			root := os.Getenv("CLAUDE_CONFIG_DIR")
			if root == "" {
				root = filepath.Join(home, ".claude")
			}
			files = append(files, filepath.Join(root, "settings.json"))
			for _, key := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_BASE_URL"} {
				environment[key] = os.Getenv(key)
			}
		}
	}
	digest, err := fingerprintFiles(files)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(struct {
		Files       string
		Command     []string
		Environment map[string]string
	}{digest, command, environment})
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}
func fingerprintFiles(files []string) (string, error) {
	h := sha256.New()
	for _, file := range files {
		fmt.Fprintf(h, "%s\x00", file)
		f, err := os.Open(file)
		if os.IsNotExist(err) {
			fmt.Fprint(h, "absent\x00")
			continue
		}
		if err != nil {
			return "", fmt.Errorf("cannot fingerprint harness configuration: %w", err)
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1000000000 {
			f.Close()
			return "", fmt.Errorf("unbounded or non-regular harness input")
		}
		n, err := io.Copy(h, io.LimitReader(f, 1000000001))
		f.Close()
		if err != nil || n > 1000000000 {
			return "", fmt.Errorf("cannot fingerprint complete harness input")
		}
		fmt.Fprint(h, "\x00")
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
