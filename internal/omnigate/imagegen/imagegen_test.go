package imagegen

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	id, err := s.Save([]byte("PNGDATA"), "image/png")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.HasSuffix(id, ".png") {
		t.Fatalf("id = %q want a .png", id)
	}
	f, mime, err := s.Open(id)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if mime != "image/png" {
		t.Fatalf("mime = %q", mime)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "PNGDATA" {
		t.Fatalf("data = %q", data)
	}
}

func TestStoreUnknownMimeFallsBackToPng(t *testing.T) {
	s := NewStore(t.TempDir())
	id, err := s.Save([]byte("x"), "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(id, ".png") {
		t.Fatalf("id = %q want a .png fallback", id)
	}
}

// TestStoreOpenRejectsTraversal: the id is the only thing a client supplies, so
// nothing but a well-formed id may reach the filesystem.
func TestStoreOpenRejectsTraversal(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, id := range []string{
		"../secret.txt",
		"..\\secret.txt",
		"/etc/passwd",
		"0123456789abcdef0123456789abcdef.exe",
		"",
		".",
	} {
		if _, _, err := s.Open(id); err == nil {
			t.Fatalf("Open(%q) succeeded, want a rejection", id)
		}
	}
}

func TestStoreNilAndEmptyDir(t *testing.T) {
	var nilStore *Store
	if _, err := nilStore.Save([]byte("x"), "image/png"); err == nil {
		t.Fatal("nil store saved")
	}
	if _, _, err := nilStore.Open("0123456789abcdef0123456789abcdef.png"); err == nil {
		t.Fatal("nil store opened")
	}
	if got := nilStore.Sweep(time.Now()); got != 0 {
		t.Fatalf("nil store swept %d", got)
	}
	empty := NewStore("")
	if _, err := empty.Save([]byte("x"), "image/png"); err == nil {
		t.Fatal("empty-dir store saved")
	}
}

func TestStoreSweepDropsOldFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	fresh, err := s.Save([]byte("new"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := s.Save([]byte("old"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	// Age one file past the retention window.
	old := time.Now().Add(-Retention - time.Hour)
	if err := os.Chtimes(filepath.Join(dir, stale), old, old); err != nil {
		t.Fatal(err)
	}
	if removed := s.Sweep(time.Now()); removed != 1 {
		t.Fatalf("removed = %d want 1", removed)
	}
	if _, _, err := s.Open(stale); err == nil {
		t.Fatal("stale image still readable")
	}
	if _, _, err := s.Open(fresh); err != nil {
		t.Fatalf("fresh image gone: %v", err)
	}
}
