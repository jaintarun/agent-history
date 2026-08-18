package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotatingWriterKeepsOneBoundedBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "service.log")
	writer, err := newRotatingWriter(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Repeat("a", 23) + "\n"
	second := strings.Repeat("b", 23) + "\n"
	if _, err := writer.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(second)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileContentAndMode(t, path+".1", first, 0o600)
	assertFileContentAndMode(t, path, second, 0o600)
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Fatalf("second backup exists: %v", err)
	}
}

func TestRotatingWriterReplacesPreviousBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	writer, err := newRotatingWriter(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []string{"first-record\n", "second-record\n", "third-record\n"} {
		if _, err := writer.Write([]byte(record)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileContentAndMode(t, path+".1", "second-record\n", 0o600)
	assertFileContentAndMode(t, path, "third-record\n", 0o600)
}

func TestRotatingWriterSerializesConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	writer, err := newRotatingWriter(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	const writers = 8
	const recordsPerWriter = 50
	var wait sync.WaitGroup
	for worker := range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for record := range recordsPerWriter {
				if _, err := fmt.Fprintf(writer, "worker=%d record=%d\n", worker, record); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		counts[line]++
	}
	for worker := range writers {
		for record := range recordsPerWriter {
			line := fmt.Sprintf("worker=%d record=%d", worker, record)
			if counts[line] != 1 {
				t.Fatalf("record %q count = %d", line, counts[line])
			}
		}
	}
}

func TestServiceLogWriterUsesConfiguredPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	t.Setenv("AGENT_HISTORY_LOG_PATH", path)
	fallback := &bytes.Buffer{}
	writer, closer := serviceLogWriter(fallback)
	if _, err := writer.Write([]byte("structured service log\n")); err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileContentAndMode(t, path, "structured service log\n", 0o600)
	if fallback.Len() != 0 {
		t.Fatalf("fallback log = %q", fallback.String())
	}
}

func assertFileContentAndMode(t *testing.T, path, want string, mode os.FileMode) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != want {
		t.Fatalf("%s content = %q, want %q", path, content, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != mode {
		t.Fatalf("%s mode = %o, want %o", path, got, mode)
	}
}
