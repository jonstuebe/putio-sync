package putiosync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearPutioEnv removes any PUTIO_ variables the machine running the tests
// happens to have set, so a developer's own credentials do not decide whether
// the suite passes.
func clearPutioEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "PUTIO_") {
			continue
		}
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Setenv(key, value) })
	}
}

func TestReadDefaults(t *testing.T) {
	clearPutioEnv(t)

	var c Config
	if err := c.Read(filepath.Join(t.TempDir(), "no-such-config.toml")); err != nil {
		t.Fatal(err)
	}
	if c.LocalDir != "~/putio-sync" {
		t.Errorf("LocalDir = %q", c.LocalDir)
	}
	if c.Concurrency != defaultConcurrency {
		t.Errorf("Concurrency = %d, want %d", c.Concurrency, defaultConcurrency)
	}
}

func TestReadFromFileAndEnvironment(t *testing.T) {
	clearPutioEnv(t)

	path := filepath.Join(t.TempDir(), "config.toml")
	err := os.WriteFile(path, []byte("Username = \"from-file\"\nConcurrency = 2\n"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	// The spelling documented in the README.
	t.Setenv("PUTIO_Concurrency", "8")
	t.Setenv("PUTIO_LocalDir", "/tmp/elsewhere")

	var c Config
	if err := c.Read(path); err != nil {
		t.Fatal(err)
	}
	if c.Username != "from-file" {
		t.Errorf("Username = %q, want the value from the config file", c.Username)
	}
	if c.Concurrency != 8 {
		t.Errorf("Concurrency = %d, want the environment to win over the file", c.Concurrency)
	}
	if c.LocalDir != "/tmp/elsewhere" {
		t.Errorf("LocalDir = %q, want the environment value", c.LocalDir)
	}
}

func TestConcurrencyIsAtLeastOne(t *testing.T) {
	clearPutioEnv(t)

	t.Setenv("PUTIO_Concurrency", "-3")
	var c Config
	if err := c.Read(filepath.Join(t.TempDir(), "no-such-config.toml")); err != nil {
		t.Fatal(err)
	}
	if c.Concurrency != 1 {
		t.Errorf("Concurrency = %d, want 1: a sync that transfers nothing is not a useful setting", c.Concurrency)
	}
}
