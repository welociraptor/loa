package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionLine(t *testing.T) {
	oldVersion, oldCommit := version, commit
	t.Cleanup(func() {
		version, commit = oldVersion, oldCommit
	})

	tests := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{"release", "1.2.3", "570563f", "loa 1.2.3 570563f"},
		{"dirty", "1.2.3", "570563f-dirty", "loa 1.2.3 570563f-dirty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version, commit = tt.version, tt.commit
			if got := versionLine(); got != tt.want {
				t.Errorf("versionLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVersionNeedsNoStoreOrSession(t *testing.T) {
	c := newCLI(t)
	c.dir = t.TempDir()

	out, errOut, code := c.run("version")
	if code != 0 {
		t.Fatalf("version exited %d: %s", code, errOut)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("version did not end its line: %q", out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "loa ") {
		t.Errorf("version = %q, want one line beginning \"loa \"", out)
	}
	for _, path := range []string{filepath.Join(c.dir, "loa.db"), c.db} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("version touched %s: stat returned %v", path, err)
		}
	}
}
