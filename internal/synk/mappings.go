package synk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

// Mapping bindings prevent a changed configuration from reinterpreting history.
// Legacy state is deliberately left untouched; switching a populated legacy
// mirror to mappings requires a fresh target directory.
func (s *State) checkMappings(cfg *config.Config, bind bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	mappings := append([]config.DriveMapping(nil), cfg.DriveSubfolders...)
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].Local < mappings[j].Local })
	if len(s.data.Mappings) > 0 {
		validPath := func(p string) bool {
			for _, part := range strings.Split(p, "/") {
				if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
					return false
				}
			}
			for _, m := range mappings {
				if mappingContains(m, p) {
					return true
				}
			}
			return false
		}
		for p := range s.data.Drive {
			if !validPath(p) {
				return fmt.Errorf("state path %q is outside configured mappings; state preserved", p)
			}
		}
		for _, a := range s.data.Artifacts {
			if !validPath(a.Original) {
				return fmt.Errorf("artifact path %q is outside configured mappings; state preserved", a.Original)
			}
		}
		if !reflect.DeepEqual(mappings, s.data.Mappings) {
			return fmt.Errorf("drive_subfolders differs from persisted drive_mappings; use a separate target_dir to change mappings safely")
		}
	} else if len(mappings) > 0 {
		if len(s.data.Drive) > 0 || len(s.data.Artifacts) > 0 {
			return fmt.Errorf("existing legacy Drive state cannot be reinterpreted as drive_subfolders; use a fresh target_dir (existing state is preserved)")
		}
		if bind {
			s.data.Mappings = mappings
			s.dirty = true
			s.revision++
		}
	}
	return nil
}

func (e *Engine) mappingRoot(p string) bool {
	for _, m := range e.cfg.DriveSubfolders {
		if p == m.Local {
			return true
		}
	}
	return false
}

func (e *Engine) fetchMappings(ctx context.Context) (map[string]icloud.Item, map[string]icloud.Item, error) {
	if e.cfg.DriveSubfolders == nil {
		return e.fetchRemote(ctx)
	}
	files, dirs := map[string]icloud.Item{}, map[string]icloud.Item{}
	identities := map[string]string{}
	for _, m := range e.cfg.DriveSubfolders {
		cfg := *e.cfg
		cfg.DriveSubfolders = nil
		cfg.DriveSubfolder = m.Remote
		view := &Engine{cfg: &cfg, cl: e.cl, scanner: e.scanner}
		ff, dd, err := view.fetchRemote(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("mapping %q -> %q: %w", m.Remote, m.Local, err)
		}
		for p, item := range dd {
			if previous, ok := identities[item.DrivewsID]; ok {
				return nil, nil, fmt.Errorf("remote mappings share folder %q (%s and %s); refusing to sync", item.DrivewsID, previous, m.Remote)
			}
			identities[item.DrivewsID] = m.Remote
			key := m.Local
			if p != "" {
				key += "/" + p
			}
			dirs[key] = item
		}
		for p, item := range ff {
			files[m.Local+"/"+p] = item
		}
	}
	return files, dirs, nil
}

// Reject symlinks in roots as well as inventories. A skipped symlink must never
// be mistaken for a missing local file and subsequently replaced by a download.
func noSymlinkTree(root string) error {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	for p := absolute; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && p == absolute && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("synchronization root is not a directory: %s", p)
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in synchronization path: %s", p)
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) && p == root {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in synchronization tree: %s", p)
		}
		return nil
	})
}

func (e *Engine) scanMappings() (map[string]LocalFile, []string, []string, error) {
	if e.cfg.DriveSubfolders == nil {
		return e.scanner.Scan()
	}
	files := map[string]LocalFile{}
	var skipped, oversized []string
	for _, m := range e.cfg.DriveSubfolders {
		root := filepath.Join(e.cfg.DriveDir(), filepath.FromSlash(m.Local))
		if err := noSymlinkTree(root); err != nil {
			return nil, nil, nil, err
		}
		scanner := &Scanner{Root: root, Exclude: e.cfg.Exclude, MaxSize: e.cfg.MaxFileSize}
		// Missing local roots represent an empty initial mirror. They are created
		// only after every remote inventory has succeeded.
		if _, err := os.Lstat(root); os.IsNotExist(err) {
			continue
		}
		ff, ss, oo, err := scanner.Scan()
		if err != nil {
			return nil, nil, nil, err
		}
		for p, f := range ff {
			f.Path = m.Local + "/" + p
			files[f.Path] = f
		}
		for _, p := range ss {
			skipped = append(skipped, m.Local+"/"+p)
		}
		for _, p := range oo {
			oversized = append(oversized, m.Local+"/"+p)
		}
	}
	return files, skipped, oversized, nil
}

func prefixMessages(dst *[]string, src []string, prefix string) {
	for _, message := range src {
		*dst = append(*dst, "["+prefix+"] "+message)
	}
}

func verifyMappings(ctx context.Context, cfg *config.Config, cl driveVerifyClient) (VerifyReport, error) {
	var result VerifyReport
	if err := cfg.ValidateDriveMappings(); err != nil {
		return result, err
	}
	st, err := LoadState(cfg.StatePath())
	if err != nil {
		return result, err
	}
	if err := st.checkMappings(cfg, false); err != nil {
		return result, err
	}
	for _, m := range cfg.DriveSubfolders {
		view := *cfg
		view.DriveSubfolders = nil
		view.DriveSubfolder = m.Remote
		if err := noSymlinkTree(filepath.Join(cfg.DriveDir(), filepath.FromSlash(m.Local))); err != nil {
			return result, err
		}
		r, err := verifyDriveSingle(ctx, &view, cl, m.Local)
		if err != nil {
			return result, fmt.Errorf("mapping %s -> %s: %w", m.Remote, m.Local, err)
		}
		result.LocalFiles += r.LocalFiles
		result.RemoteFiles += r.RemoteFiles
		result.Identical += r.Identical
		label := m.Remote + " -> " + m.Local
		prefixMessages(&result.Differences, r.Differences, label)
		prefixMessages(&result.Unverifiable, r.Unverifiable, label)
		prefixMessages(&result.SkippedInternal, r.SkippedInternal, label)
		prefixMessages(&result.SkippedLocal, r.SkippedLocal, label)
	}
	return result, nil
}

func mappingContains(m config.DriveMapping, p string) bool { return strings.HasPrefix(p, m.Local+"/") }
