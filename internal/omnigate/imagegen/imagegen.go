// Package imagegen stores AI-generated images on disk and hands them back by id.
//
// A chat completion body can only carry text, so a generated image is surfaced as
// a markdown link to a URL the client can fetch. The bytes live under the data
// dir (a bind mount, so they survive a restart) and are served by the gateway's
// /images/{id} route.
//
// The id is the only thing a client ever supplies, so it is validated against a
// strict pattern before it reaches the filesystem: no path separators, no "..",
// nothing but 32 hex characters and a known extension.
package imagegen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Retention is how long a stored image stays fetchable. Generated images are
// only useful while the conversation that produced them is alive; a week matches
// the upstream default.
const Retention = 7 * 24 * time.Hour

// extByMime maps an upstream mime type to a file extension. Anything unknown
// falls back to png.
var extByMime = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
	"image/gif":  "gif",
}

// mimeByExt is the inverse, used when serving a stored file.
var mimeByExt = map[string]string{
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"webp": "image/webp",
	"gif":  "image/gif",
}

// idRE is the only shape Save produces: 32 hex chars plus a known extension.
var idRE = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|jpeg|webp|gif)$`)

// Store is a directory-backed image store. A nil Store (or one with an empty
// dir) stores nothing and serves nothing — used when no data dir is configured.
type Store struct{ dir string }

// NewStore returns a store rooted at dir. dir is created on first Save.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Dir returns the backing directory ("" when unset).
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Save writes data and returns the file id ("<32 hex>.<ext>"). The mime type
// only selects the extension; it is not validated against the bytes.
func (s *Store) Save(data []byte, mime string) (string, error) {
	if s == nil || s.dir == "" {
		return "", fmt.Errorf("imagegen: 未配置存储目录")
	}
	if len(data) == 0 {
		return "", fmt.Errorf("imagegen: 空图片")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", err
	}
	ext := extByMime[strings.ToLower(strings.TrimSpace(mime))]
	if ext == "" {
		ext = "png"
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:]) + "." + ext
	if err := os.WriteFile(filepath.Join(s.dir, id), data, 0o644); err != nil {
		return "", err
	}
	// Opportunistic retention: image generation is rare, so one directory scan
	// per stored image is cheaper than a goroutine that mostly sleeps.
	s.Sweep(time.Now())
	return id, nil
}

// Open returns the stored image and its content type. The id is validated
// against idRE first, so "..", absolute paths and separators cannot reach the
// filesystem. A missing or malformed id reports os.ErrNotExist.
func (s *Store) Open(id string) (io.ReadSeekCloser, string, error) {
	if s == nil || s.dir == "" || !idRE.MatchString(id) {
		return nil, "", os.ErrNotExist
	}
	f, err := os.Open(filepath.Join(s.dir, id))
	if err != nil {
		return nil, "", err
	}
	ext := id[strings.LastIndex(id, ".")+1:]
	mime := mimeByExt[ext]
	if mime == "" {
		mime = "image/png"
	}
	return f, mime, nil
}

// Sweep deletes stored images older than Retention and returns how many went. It
// is called opportunistically (once per Save) rather than on a timer: image
// generation is rare, so a directory scan per image is cheaper than a goroutine
// that mostly sleeps.
func (s *Store) Sweep(now time.Time) int {
	if s == nil || s.dir == "" {
		return 0
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > Retention {
			if os.Remove(filepath.Join(s.dir, e.Name())) == nil {
				removed++
			}
		}
	}
	return removed
}
