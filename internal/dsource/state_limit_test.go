package dsource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadStateLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "terraform.tfstate")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readState(path, 100); err != nil || len(data) != 100 {
		t.Fatalf("at the limit: %d, %v", len(data), err)
	}
	if _, err := readState(path, 99); err == nil || !strings.Contains(err.Error(), "-max-state-size") {
		t.Fatalf("above the limit: %v", err)
	}
}
