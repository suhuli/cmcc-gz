package logx

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRingAndLevels(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.log")
	ring := Setup(Options{Level: "INFO", File: f})
	t.Cleanup(Close)
	slog.Debug("hidden")
	slog.Info("hello", "k", "v")
	slog.Warn("careful")
	lines := ring.Tail(10)
	if len(lines) != 2 || !strings.Contains(lines[0], " INFO hello k=v") || !strings.Contains(lines[1], " WARNING careful") {
		t.Fatalf("lines = %q", lines)
	}
	SetLevel("DEBUG")
	slog.Debug("shown")
	if l := ring.Tail(1); !strings.Contains(l[0], "DEBUG shown") {
		t.Fatalf("debug = %q", l)
	}
	raw, _ := os.ReadFile(f)
	if !strings.Contains(string(raw), "hello") {
		t.Fatal("file not written")
	}
}

func TestRingWraps(t *testing.T) {
	r := NewRing(3)
	for i := 0; i < 5; i++ {
		r.add(fmt.Sprint(i))
	}
	if got := strings.Join(r.Tail(10), ","); got != "2,3,4" {
		t.Fatalf("tail = %s", got)
	}
}

func TestRotation(t *testing.T) {
	f := filepath.Join(t.TempDir(), "r.log")
	w, err := openRotating(f, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 50; i++ {
		_, _ = w.Write([]byte("0123456789\n"))
	}
	if _, err := os.Stat(f + ".1"); err != nil {
		t.Fatal("no rotation")
	}
	if _, err := os.Stat(f + ".3"); err == nil {
		t.Fatal("too many backups")
	}
}
