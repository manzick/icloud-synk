package synk

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

// ReplacementArtifact records observed identity, not inferred name ownership.
// Pending artifacts must never be collected, including after a restart.
// Version is independent of the backward-compatible enclosing state format.
type ReplacementArtifact struct {
	Version  int         `json:"version"`
	Original string      `json:"original"`
	ParentID string      `json:"parent_id"`
	Kind     string      `json:"kind"`
	Item     icloud.Item `json:"item"`
	Created  time.Time   `json:"created"`
	Sequence uint64      `json:"sequence"`
	Complete bool        `json:"complete"`
}

func (s *State) Artifacts() map[string]ReplacementArtifact {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]ReplacementArtifact, len(s.data.Artifacts))
	for id, a := range s.data.Artifacts {
		out[id] = a
	}
	return out
}

func (s *State) putArtifact(a ReplacementArtifact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Artifacts == nil {
		s.data.Artifacts = map[string]ReplacementArtifact{}
	}
	if a.Sequence == 0 {
		// Order observed creations independently of wall-clock adjustments.
		for _, previous := range s.data.Artifacts {
			if previous.Sequence > a.Sequence {
				a.Sequence = previous.Sequence
			}
		}
		a.Sequence++
	}
	s.data.Artifacts[a.Item.DrivewsID] = a
	s.dirty = true
	s.revision++
}

func (s *State) deleteArtifact(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Artifacts, id)
	s.dirty = true
	s.revision++
}

func (e *Engine) recordArtifact(original string, parent, item icloud.Item, kind string) error {
	e.st.putArtifact(ReplacementArtifact{Version: 1, Original: original, ParentID: parent.DrivewsID,
		Kind: kind, Item: item, Created: time.Now().UTC()})
	if err := e.st.Save(); err != nil {
		return fmt.Errorf("persisting replacement artifact ownership: %w", err)
	}
	return nil
}

// artifactMatches requires an exact, unchanged identity in its observed parent.
// The name format is an extra consistency check, never evidence of ownership.
func artifactMatches(a ReplacementArtifact, originalParent, parentID string, item icloud.Item) bool {
	return a.Version == 1 && a.Original != "" && path.Dir(a.Original) == originalParent &&
		a.ParentID != "" && a.ParentID == parentID &&
		(a.Kind == "backup" || a.Kind == "new" || a.Kind == "failed") &&
		artifactNameMatches(a) && sameArtifactItem(a.Item, item)
}

func artifactNameMatches(a ReplacementArtifact) bool {
	base := path.Base(a.Original)
	ext := path.Ext(base)
	prefix := strings.TrimSuffix(base, ext) + ".icloud-synk-" + a.Kind + "-"
	name := a.Item.FullName()
	if !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+25+len(ext) || !isReplacementSiblingName(name) {
		return false
	}
	return name == replacementSiblingName(base, a.Kind, name[len(prefix):len(prefix)+25])
}

func sameArtifactItem(a, b icloud.Item) bool {
	return a.Type == icloud.TypeFile && b.Type == icloud.TypeFile && a.ItemCount == 0 && b.ItemCount == 0 &&
		a.DrivewsID != "" && a.DrivewsID == b.DrivewsID && a.DocwsID != "" && a.DocwsID == b.DocwsID &&
		a.Zone != "" && a.Zone == b.Zone && a.Etag != "" && a.Etag == b.Etag &&
		a.Size == b.Size && a.FullName() == b.FullName() && a.ModTime().Equal(b.ModTime())
}

// cleanupBackups runs only after transfers have finished. A pending replacement
// blocks collection for its original path. Historical/unowned files are ignored.
func (e *Engine) cleanupBackups(ctx context.Context) error {
	if e.dryRun || e.cfg.BackupRetention == -1 {
		return nil
	}
	if e.cfg.BackupRetention < -1 {
		return fmt.Errorf("invalid backup_retention: %d", e.cfg.BackupRetention)
	}
	groups := map[string][]ReplacementArtifact{}
	blocked := map[string]bool{}
	for _, a := range e.st.Artifacts() {
		if a.Version != 1 || !a.Complete {
			blocked[a.Original] = true
			continue
		}
		if a.Kind == "backup" {
			groups[a.Original] = append(groups[a.Original], a)
		}
	}
	var paths []string
	for p := range groups {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var errs []error
	for _, p := range paths {
		if blocked[p] {
			e.log.Warn("backup retention deferred; unfinished replacement artifacts retained", "path", p)
			continue
		}
		if err := e.cleanupFileBackups(ctx, p, groups[p]); err != nil {
			e.log.Warn("backup cleanup incomplete; synchronized content and remaining backups retained", "path", p, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) cleanupFileBackups(ctx context.Context, original string, backups []ReplacementArtifact) error {
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].Sequence != backups[j].Sequence {
			return backups[i].Sequence > backups[j].Sequence
		}
		return backups[i].Item.DrivewsID < backups[j].Item.DrivewsID
	})
	parentRel := path.Dir(original)
	if parentRel == "." {
		parentRel = ""
	}
	parent, ok := e.lookupDir(parentRel)
	if !ok {
		return fmt.Errorf("remote parent unavailable")
	}
	baseline, ok := e.st.Drive()[original]
	if !ok || baseline.SHA256 == "" || baseline.Etag == "" {
		return fmt.Errorf("verified target baseline unavailable")
	}
	// Save provenance/baseline before issuing any trash request.
	if err := e.st.Save(); err != nil {
		return err
	}
	items, err := e.remoteChildren(ctx, parent)
	if err != nil {
		return err
	}
	var eligible []ReplacementArtifact
	for _, a := range backups {
		if a.ParentID != parent.DrivewsID {
			return fmt.Errorf("backup belongs to a different remote parent; preserved")
		}
		var found *icloud.Item
		for i := range items {
			if items[i].DrivewsID == a.Item.DrivewsID {
				found = &items[i]
				break
			}
		}
		if found == nil {
			// An earlier trash request may have succeeded despite a lost response.
			e.log.Info("owned backup no longer listed; forgetting artifact record", "original", original, "backup", a.Item.FullName())
			e.st.deleteArtifact(a.Item.DrivewsID)
			continue
		}
		if !artifactMatches(a, path.Dir(original), parent.DrivewsID, *found) {
			return fmt.Errorf("backup %q changed or ownership no longer matches; preserved", a.Item.FullName())
		}
		eligible = append(eligible, a)
	}
	if err := e.st.Save(); err != nil {
		return err
	}
	// Remove oldest first so partial cleanup retains the most recent recovery
	// versions, in addition to the N versions explicitly kept.
	for i := len(eligible) - 1; i >= e.cfg.BackupRetention; i-- {
		a := eligible[i]
		// Re-list before EACH deletion. The target must still be the verified
		// version, and the candidate must still be the artifact we recorded.
		items, err := e.remoteChildren(ctx, parent)
		if err != nil {
			return err
		}
		for _, name := range []string{path.Base(original), a.Item.FullName()} {
			matches := 0
			for _, it := range items {
				if it.FullName() == name {
					matches++
				}
			}
			if matches != 1 {
				return fmt.Errorf("remote name %q is absent or ambiguous; backups preserved", name)
			}
		}
		target := findRemoteItem(items, path.Base(original))
		if target == nil || baseline.DrivewsID == "" || baseline.DocwsID == "" || baseline.Zone == "" ||
			remoteChanged(*target, baseline, true) || target.Type != icloud.TypeFile || target.ItemCount != 0 {
			return fmt.Errorf("replacement target changed or is missing; backups preserved")
		}
		if err := e.verifyRemoteHash(ctx, *target, baseline.SHA256); err != nil {
			return fmt.Errorf("replacement target no longer verifies; backups preserved: %w", err)
		}
		candidate := findRemoteItem(items, a.Item.FullName())
		if candidate == nil || !artifactMatches(a, path.Dir(original), parent.DrivewsID, *candidate) || candidate.DrivewsID == target.DrivewsID {
			return fmt.Errorf("backup identity changed; preserved %q", a.Item.FullName())
		}
		e.log.Info("moving owned replacement backup to iCloud trash", "original", original, "backup", candidate.FullName(), "drivewsid", candidate.DrivewsID)
		if err := e.cl.MoveToTrash(ctx, []icloud.Item{*candidate}); err != nil {
			return err
		}
		remaining, err := e.remoteChildren(ctx, parent)
		if err != nil {
			return fmt.Errorf("confirming backup cleanup: %w", err)
		}
		for _, it := range remaining {
			if it.DrivewsID == candidate.DrivewsID {
				return fmt.Errorf("backup trash operation was not confirmed")
			}
		}
		e.log.Info("backup cleanup confirmed", "original", original, "backup", candidate.FullName())
		e.st.deleteArtifact(candidate.DrivewsID)
		if err := e.st.Save(); err != nil {
			return err
		}
	}
	return nil
}
