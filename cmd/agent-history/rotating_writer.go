package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const serviceLogMaxBytes = 8 << 20

type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

func newRotatingWriter(path string, maxBytes int64) (*rotatingWriter, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("log size limit must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	writer := &rotatingWriter{path: path, maxBytes: maxBytes}
	if err := writer.open(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close service log for rotation: %w", err)
	}
	w.file = nil
	backup := w.path + ".1"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove previous service log backup: %w", err)
	}
	if err := os.Rename(w.path, backup); err != nil {
		return fmt.Errorf("rotate service log: %w", err)
	}
	if err := w.open(); err != nil {
		return err
	}
	return nil
}

func (w *rotatingWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open service log: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure service log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat service log: %w", err)
	}
	w.file = file
	w.size = info.Size()
	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func serviceLogWriter(fallback io.Writer) (io.Writer, io.Closer) {
	path := os.Getenv("AGENT_HISTORY_LOG_PATH")
	if path == "" {
		return nopWriteCloser{fallback}, nopWriteCloser{fallback}
	}
	writer, err := newRotatingWriter(path, serviceLogMaxBytes)
	if err != nil {
		fmt.Fprintf(fallback, "agent-history: could not open service log: %v\n", err)
		return nopWriteCloser{fallback}, nopWriteCloser{fallback}
	}
	return writer, writer
}
