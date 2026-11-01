package synk

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// mappedFS confines mapped Drive operations to target_dir even if a path is
// replaced by a symlink after inventory. Legacy operations retain their API.
type mappedFS struct {
	root *os.Root
	base string
}

func (f *mappedFS) rel(p string) string {
	r, err := filepath.Rel(f.base, p)
	if err != nil {
		return "../invalid"
	}
	return r
}
func (e *Engine) mkdirAll(p string, mode os.FileMode) error {
	if e.fs == nil {
		return os.MkdirAll(p, mode)
	}
	return e.fs.root.MkdirAll(e.fs.rel(p), mode)
}
func (e *Engine) stat(p string) (os.FileInfo, error) {
	if e.fs == nil {
		return os.Stat(p)
	}
	return e.fs.root.Stat(e.fs.rel(p))
}
func (e *Engine) open(p string) (*os.File, error) {
	if e.fs == nil {
		return os.Open(p)
	}
	return e.fs.root.Open(e.fs.rel(p))
}
func (e *Engine) remove(p string) error {
	if e.fs == nil {
		return os.Remove(p)
	}
	return e.fs.root.Remove(e.fs.rel(p))
}
func (e *Engine) removeAll(p string) error {
	if e.fs == nil {
		return os.RemoveAll(p)
	}
	return e.fs.root.RemoveAll(e.fs.rel(p))
}
func (e *Engine) rename(a, b string) error {
	if e.fs == nil {
		return renameFile(a, b)
	}
	return e.fs.root.Rename(e.fs.rel(a), e.fs.rel(b))
}
func (e *Engine) link(a, b string) error {
	if e.fs == nil {
		return os.Link(a, b)
	}
	return e.fs.root.Link(e.fs.rel(a), e.fs.rel(b))
}
func (e *Engine) chmod(p string, mode os.FileMode) error {
	if e.fs == nil {
		return os.Chmod(p, mode)
	}
	f, err := e.open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(mode)
}
func (e *Engine) createTemp(dir, pattern string) (*os.File, error) {
	if e.fs == nil {
		return os.CreateTemp(dir, pattern)
	}
	for i := 0; i < 100; i++ {
		name := strings.Replace(pattern, "*", rand.Text(), 1)
		f, err := e.fs.root.OpenFile(e.fs.rel(filepath.Join(dir, name)), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	return nil, fmt.Errorf("cannot allocate temporary file")
}
func (e *Engine) hashFile(p string) (string, error) {
	f, err := e.open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(f)
}

func (e *Engine) createLocal(p string) (*os.File, error) {
	if e.fs == nil {
		return os.Create(p)
	}
	return e.fs.root.Create(e.fs.rel(p))
}
func (e *Engine) setTimes(p string, t time.Time) error {
	// Use the opened file descriptor: Root.Chtimes has a symlink race on Unix.
	if e.fs != nil {
		f, err := e.open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return setFileTimes(f, t)
	}
	return os.Chtimes(p, t, t)
}
func hashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
