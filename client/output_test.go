package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplaceHandlesWhateverIsAlreadyThere(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, output string)
	}{
		{"nothing there", func(t *testing.T, output string) {}},
		{"a file", func(t *testing.T, output string) {
			if err := os.WriteFile(output, []byte("previous"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a directory with content", func(t *testing.T, output string) {
			if err := os.MkdirAll(filepath.Join(output, "nested"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(output, "nested", "file"), []byte("previous"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "out")
			test.prepare(t, output)

			staged := filepath.Join(dir, "staged")
			if err := os.WriteFile(staged, []byte("replacement"), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := replace(context.Background(), staged, output); err != nil {
				t.Fatalf("replace: %v", err)
			}

			got, err := os.ReadFile(output)
			if err != nil {
				t.Fatalf("reading the replaced output: %v", err)
			}
			if string(got) != "replacement" {
				t.Errorf("output = %q, want the staged content", got)
			}
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Error("the staging path survived the move")
			}
		})
	}
}

// A caller waiting on another download's replace must not wait past its own
// deadline.
func TestOutputLockGivesUpWithTheCaller(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "out")

	held, err := outputs.lock(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	defer held()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := outputs.lock(ctx, output); err == nil {
		t.Fatal("a second holder was granted the same path")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s for a 100ms deadline", elapsed.Round(time.Millisecond))
	}
}

// Locks are per path, so unrelated downloads never wait on each other.
func TestOutputLocksAreIndependent(t *testing.T) {
	dir := t.TempDir()

	first, err := outputs.lock(context.Background(), filepath.Join(dir, "one"))
	if err != nil {
		t.Fatal(err)
	}
	defer first()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	second, err := outputs.lock(ctx, filepath.Join(dir, "two"))
	if err != nil {
		t.Fatalf("a different path had to wait: %v", err)
	}
	second()

	// Released entries are dropped, so the table does not grow with every path.
	outputs.mutex.Lock()
	remaining := len(outputs.held)
	outputs.mutex.Unlock()

	if remaining != 1 {
		t.Errorf("%d paths still tracked, want only the one still held", remaining)
	}
}

// A download abandoned at its deadline is reported as failed, so it must not
// publish afterwards over whatever the caller kept.
func TestReplaceDoesNothingPastTheDeadline(t *testing.T) {
	dir := t.TempDir()

	output := filepath.Join(dir, "out")
	if err := os.WriteFile(output, []byte("the caller's own"), 0o600); err != nil {
		t.Fatal(err)
	}

	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, []byte("late arrival"), 0o600); err != nil {
		t.Fatal(err)
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	if err := replace(expired, staged, output); err == nil {
		t.Error("a replace past the deadline was allowed")
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the caller's own" {
		t.Errorf("output = %q, want it untouched", got)
	}
}
