package synk

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

type retentionClient struct {
	*downloadTestClient
	trashed     []string
	trashErr    error
	keepTrashed bool
	parents     map[string]string
	afterRename func(string)
}

func (c *retentionClient) Rename(ctx context.Context, item icloud.Item, name string) error {
	if err := c.downloadTestClient.Rename(ctx, item, name); err != nil {
		return err
	}
	if c.afterRename != nil {
		c.afterRename(name)
	}
	return nil
}

func (c *retentionClient) Upload(ctx context.Context, zone, parentDoc, name string, size int64, mt time.Time, r io.Reader) error {
	if err := c.downloadTestClient.Upload(ctx, zone, parentDoc, name, size, mt, r); err != nil {
		return err
	}
	c.remoteItems[len(c.remoteItems)-1].ParentID = c.parents[parentDoc]
	return nil
}

func (c *retentionClient) ListFolders(_ context.Context, ids []string) ([]icloud.Folder, error) {
	var folders []icloud.Folder
	for _, id := range ids {
		f := icloud.Folder{Item: icloud.Item{DrivewsID: id, Type: icloud.TypeFolder}}
		for _, it := range c.remoteItems {
			if it.ParentID == id || (it.ParentID == "" && id == "root") {
				f.Items = append(f.Items, it)
			}
		}
		folders = append(folders, f)
	}
	return folders, nil
}

func (c *retentionClient) MoveToTrash(_ context.Context, items []icloud.Item) error {
	if c.trashErr != nil {
		return c.trashErr
	}
	for _, it := range items {
		c.trashed = append(c.trashed, it.DrivewsID)
		if c.keepTrashed {
			continue
		}
		for i, current := range c.remoteItems {
			if current.DrivewsID == it.DrivewsID {
				if current.Etag != it.Etag {
					return fmt.Errorf("etag mismatch")
				}
				c.remoteItems = append(c.remoteItems[:i], c.remoteItems[i+1:]...)
				break
			}
		}
	}
	return nil
}

func retentionFixture(t *testing.T, retention int) (*Engine, *retentionClient) {
	t.Helper()
	cfg := config.Defaults()
	cfg.TargetDir, cfg.BackupRetention = t.TempDir(), retention
	st, err := LoadState(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(cfg, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	if err := e.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	c := &retentionClient{parents: map[string]string{"root-doc": "root"}, downloadTestClient: &downloadTestClient{remoteItems: []icloud.Item{}, remoteContent: map[string]string{}}}
	e.cl = c
	e.putDir("", icloud.Item{Type: icloud.TypeFolder, DrivewsID: "root", DocwsID: "root-doc", Zone: icloud.DefaultZone})
	return e, c
}

func replaceForRetention(t *testing.T, e *Engine, c *retentionClient, original, content string) {
	t.Helper()
	parentRel := path.Dir(original)
	if parentRel == "." {
		parentRel = ""
	}
	parent, ok := e.lookupDir(parentRel)
	if !ok {
		parent = icloud.Item{Type: icloud.TypeFolder, DrivewsID: "folder-" + parentRel, DocwsID: "folder-doc-" + parentRel, Zone: icloud.DefaultZone}
		e.putDir(parentRel, parent)
	}
	c.parents[parent.DocwsID] = parent.DrivewsID
	children, err := e.remoteChildren(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	old := findRemoteItem(children, path.Base(original))
	if old == nil {
		it := remoteFile(path.Base(original), "initial", 3, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		it.DrivewsID, it.DocwsID, it.ParentID = "original-"+original, "doc-"+original, parent.DrivewsID
		c.remoteItems = append(c.remoteItems, it)
		c.remoteContent[it.DrivewsID] = "old"
		old = &it
	}
	if err := os.MkdirAll(filepath.Dir(e.localPath(original)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.localPath(original), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.upload(context.Background(), Op{Kind: OpUpload, Path: original, Remote: *old}, "test"); err != nil {
		t.Fatal(err)
	}
}

func completedBackups(e *Engine, original string) []ReplacementArtifact {
	var out []ReplacementArtifact
	for _, a := range e.st.Artifacts() {
		if a.Original == original && a.Complete && a.Kind == "backup" {
			out = append(out, a)
		}
	}
	return out
}

func TestBackupRetentionLimitsAndIndependentFiles(t *testing.T) {
	for _, limit := range []int{-1, 0, 1, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			e, c := retentionFixture(t, limit)
			for i := 0; i < 5; i++ {
				for _, original := range []string{"bot.json", "résumé 多 dots.v2.json"} {
					replaceForRetention(t, e, c, original, fmt.Sprintf("version-%d-%s", i, original))
				}
				if err := e.cleanupBackups(context.Background()); err != nil {
					t.Fatal(err)
				}
				want := i + 1
				if limit >= 0 && want > limit {
					want = limit
				}
				for _, original := range []string{"bot.json", "résumé 多 dots.v2.json"} {
					backups := completedBackups(e, original)
					if len(backups) != want {
						t.Fatalf("%s: got %d backups, want %d", original, len(backups), want)
					}
					for _, a := range backups {
						if c.remoteContent[a.Item.DrivewsID] == "old" && i >= limit && limit >= 0 {
							t.Fatal("oldest backup was retained instead of a newer version")
						}
					}
				}
			}
			calls := len(c.trashed)
			if err := e.cleanupBackups(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(c.trashed) != calls {
				t.Fatal("cleanup was not idempotent")
			}
			if limit == -1 && calls != 0 {
				t.Fatal("unlimited retention removed backups")
			}
			// Reloading provenance must preserve both ownership and convergence.
			st, err := LoadState(e.cfg.StatePath())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(st.Artifacts(), e.st.Artifacts()) {
				t.Fatal("artifact provenance did not survive reload")
			}
			e.st = st
			if err := e.cleanupBackups(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(c.trashed) != calls {
				t.Fatal("restart retried completed cleanup")
			}
		})
	}
}

func TestReplacementAndRetentionCompleteInOnePass(t *testing.T) {
	e, c := retentionFixture(t, 0)
	e.cfg.DeleteRemote, e.cfg.DeleteLocal = false, false
	old := remoteFile("a.json", "baseline", 3, time.Time{})
	c.remoteItems = append(c.remoteItems, old)
	c.remoteContent[old.DrivewsID] = "old"
	if err := os.WriteFile(e.localPath("a.json"), []byte("new content"), 0600); err != nil {
		t.Fatal(err)
	}
	stats := e.apply(context.Background(), []Op{{Kind: OpUpload, Path: "a.json", Remote: old}})
	if stats.Uploaded != 1 || stats.Errors != 0 || stats.CleanupErrors != 0 || len(c.trashed) != 1 || len(c.remoteItems) != 1 {
		t.Fatalf("upload and retention did not complete together: %+v, trash=%v", stats, c.trashed)
	}
	local, _, _, err := e.scanner.Scan()
	if err != nil {
		t.Fatal(err)
	}
	ops := Reconcile(local, map[string]icloud.Item{"a.json": c.remoteItems[0]}, e.st.Drive(), Policy{Conflict: "both", ProtectDeletions: true})
	if op := findOp(t, ops, "a.json"); op.Kind != OpRecord {
		t.Fatalf("retention broke reconciliation: %+v", op)
	}
	if stats := e.apply(context.Background(), ops); !stats.Empty() || len(c.trashed) != 1 {
		t.Fatalf("repeat pass caused work: %+v", stats)
	}
}

func TestBackupRetentionNestedAndUnownedFiles(t *testing.T) {
	e, c := retentionFixture(t, 0)
	original := "nested space/文件 name.multi.part.json"
	replaceForRetention(t, e, c, original, "new content")
	owned := completedBackups(e, original)[0]
	// Exact historical naming syntax, similar stems, and a stolen name with a
	// different ID are not ownership evidence.
	for i, name := range []string{
		replacementSiblingName(path.Base(original), "backup", "20261030T124223.559975819"),
		replacementSiblingName("文件 name.multi.part.other.json", "backup", "20261030T124223.559975819"),
		"文件 name.multi.part.icloud-synk-backup-not-a-timestamp.json",
	} {
		it := remoteFile(name, "user-etag", 3, time.Time{})
		it.DrivewsID, it.DocwsID, it.ParentID = fmt.Sprint("user-", i), fmt.Sprint("user-doc-", i), owned.Item.ParentID
		c.remoteItems = append(c.remoteItems, it)
	}
	if err := e.cleanupBackups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.trashed, []string{owned.Item.DrivewsID}) || len(c.remoteItems) != 4 {
		t.Fatalf("unrelated files affected: %v", c.trashed)
	}
}

func TestRetentionSeparatesSameBasenameInDifferentFolders(t *testing.T) {
	e, c := retentionFixture(t, 1)
	for i := 0; i < 3; i++ {
		for _, original := range []string{"folder one/file.v2.json", "文件夹/file.v2.json"} {
			replaceForRetention(t, e, c, original, fmt.Sprint("version-", i))
		}
		if err := e.cleanupBackups(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, original := range []string{"folder one/file.v2.json", "文件夹/file.v2.json"} {
		backups := completedBackups(e, original)
		if len(backups) != 1 || c.remoteContent[backups[0].Item.DrivewsID] != "version-1" {
			t.Fatalf("wrong backup retained for %s: %+v", original, backups)
		}
	}
}

func TestRetentionDoesNotAdoptAStolenBackupName(t *testing.T) {
	e, c := retentionFixture(t, 0)
	replaceForRetention(t, e, c, "file.json", "new content")
	a := completedBackups(e, "file.json")[0]
	for i, it := range c.remoteItems {
		if it.DrivewsID == a.Item.DrivewsID {
			c.remoteItems[i].DrivewsID, c.remoteItems[i].DocwsID = "user-file-id", "user-doc-id"
			c.remoteContent["user-file-id"] = "USER DATA"
		}
	}
	if err := e.cleanupBackups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.trashed) != 0 || c.remoteContent["user-file-id"] != "USER DATA" {
		t.Fatal("user file with reused backup name was deleted")
	}
}

func TestRetentionUsesJournalOrderDespiteClockChanges(t *testing.T) {
	e, c := retentionFixture(t, 1)
	for i := 0; i < 3; i++ {
		replaceForRetention(t, e, c, "a.json", fmt.Sprint("version-", i))
	}
	for _, a := range e.st.Artifacts() {
		a.Created = time.Unix(-int64(a.Sequence), 0) // intentionally reverse wall-clock order
		e.st.putArtifact(a)
	}
	if err := e.cleanupBackups(context.Background()); err != nil {
		t.Fatal(err)
	}
	backups := completedBackups(e, "a.json")
	if len(backups) != 1 || c.remoteContent[backups[0].Item.DrivewsID] != "version-1" {
		t.Fatal("newest previous version not retained")
	}
}

func TestArtifactPersistenceFailureProtectsRecoveryVersions(t *testing.T) {
	for _, stage := range []string{"before-rename", "after-verification"} {
		t.Run(stage, func(t *testing.T) {
			e, c := retentionFixture(t, 0)
			old := remoteFile("a.json", "old-etag", 3, time.Time{})
			c.remoteItems = append(c.remoteItems, old)
			c.remoteContent[old.DrivewsID] = "old"
			if err := os.WriteFile(e.localPath("a.json"), []byte("new content"), 0600); err != nil {
				t.Fatal(err)
			}
			blocker := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(blocker, nil, 0600); err != nil {
				t.Fatal(err)
			}
			brokenPath := filepath.Join(blocker, "state.json")
			if stage == "before-rename" {
				e.st.path = brokenPath
			} else {
				c.afterRename = func(name string) {
					if name == "a.json" {
						e.st.path = brokenPath
					}
				}
			}
			if _, err := e.upload(context.Background(), Op{Kind: OpUpload, Path: "a.json", Remote: old}, "test"); err == nil {
				t.Fatal("journal failure not surfaced")
			}
			if err := e.cleanupBackups(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(c.trashed) != 0 || len(c.remoteItems) != 2 || c.remoteContent[old.DrivewsID] != "old" {
				t.Fatal("recovery versions lost after persistence failure")
			}
			for _, a := range e.st.Artifacts() {
				if a.Complete {
					t.Fatal("failed persistence enabled retention")
				}
			}
		})
	}
}

func TestBackupCleanupPreservesChangedAndInterruptedArtifacts(t *testing.T) {
	for _, mode := range []string{"renamed", "etag", "identity", "parent", "target-missing", "target-content", "target-etag", "pending", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			e, c := retentionFixture(t, 0)
			replaceForRetention(t, e, c, "a.json", "new content")
			a := completedBackups(e, "a.json")[0]
			switch mode {
			case "renamed", "etag", "identity":
				for i := range c.remoteItems {
					if c.remoteItems[i].DrivewsID == a.Item.DrivewsID {
						switch mode {
						case "renamed":
							c.remoteItems[i].Name = "user renamed"
						case "etag":
							c.remoteItems[i].Etag = "user-edit"
						case "identity":
							c.remoteItems[i].DocwsID = "different-doc"
						}
					}
				}
			case "parent":
				a.ParentID = "other-parent"
				e.st.putArtifact(a)
			case "target-missing":
				for i, it := range c.remoteItems {
					if it.FullName() == "a.json" {
						c.remoteItems = append(c.remoteItems[:i], c.remoteItems[i+1:]...)
						break
					}
				}
			case "target-content":
				for _, it := range c.remoteItems {
					if it.FullName() == "a.json" {
						c.remoteContent[it.DrivewsID] = "corrupted"
					}
				}
			case "target-etag":
				for i := range c.remoteItems {
					if c.remoteItems[i].FullName() == "a.json" {
						c.remoteItems[i].Etag = "changed"
					}
				}
			case "pending":
				a.Complete = false
				e.st.putArtifact(a)
			case "dry-run":
				e.dryRun = true
			}
			before := e.st.Artifacts()
			err := e.cleanupBackups(context.Background())
			if mode != "pending" && mode != "dry-run" && err == nil {
				t.Fatal("unsafe cleanup was not diagnosed")
			}
			if len(c.trashed) != 0 || !reflect.DeepEqual(before, e.st.Artifacts()) {
				t.Fatal("protected artifact was collected")
			}
		})
	}
}

func TestBackupCleanupFailureDoesNotInvalidateSuccessfulUpload(t *testing.T) {
	for _, mode := range []string{"api-error", "unconfirmed", "lost-response"} {
		t.Run(mode, func(t *testing.T) {
			e, c := retentionFixture(t, 0)
			replaceForRetention(t, e, c, "a.json", "new content")
			baseline := e.st.Drive()["a.json"]
			var logs strings.Builder
			e.log = slog.New(slog.NewTextHandler(&logs, nil))
			if mode == "unconfirmed" {
				c.keepTrashed = true
			} else {
				c.trashErr = fmt.Errorf("mock trash failure")
			}
			stats := e.apply(context.Background(), nil)
			if stats.CleanupErrors != 1 || stats.Errors != 0 || e.st.Drive()["a.json"] != baseline || len(completedBackups(e, "a.json")) != 1 {
				t.Fatalf("cleanup failure corrupted synchronization: %+v", stats)
			}
			if !strings.Contains(logs.String(), "backup cleanup incomplete") {
				t.Fatal("cleanup failure not logged")
			}
			c.keepTrashed, c.trashErr = false, nil
			if mode == "lost-response" {
				id := completedBackups(e, "a.json")[0].Item.DrivewsID
				// Simulate a successful trash whose response was lost.
				for i, it := range c.remoteItems {
					if it.DrivewsID == id {
						c.remoteItems = append(c.remoteItems[:i], c.remoteItems[i+1:]...)
						break
					}
				}
			}
			if err := e.cleanupBackups(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(completedBackups(e, "a.json")) != 0 {
				t.Fatal("cleanup did not recover")
			}
		})
	}
}

func TestFailedReplacementIsNeverEligibleForRetention(t *testing.T) {
	e, c := retentionFixture(t, 0)
	c.downloadOverrideName, c.downloadOverrideContent = "a.json", "corrupted"
	old := remoteFile("a.json", "old-etag", 3, time.Time{})
	c.remoteItems = append(c.remoteItems, old)
	c.remoteContent[old.DrivewsID] = "old"
	if err := os.WriteFile(e.localPath("a.json"), []byte("new content"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := e.upload(context.Background(), Op{Kind: OpUpload, Path: "a.json", Remote: old}, "test")
	if err == nil {
		t.Fatal("corrupt replacement accepted")
	}
	if err := e.cleanupBackups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.trashed) != 0 || len(c.remoteItems) != 2 {
		t.Fatal("failed replacement recovery copies removed")
	}
	st, err := LoadState(e.cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	e.st = st
	if err := e.cleanupBackups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.trashed) != 0 {
		t.Fatal("restart collected interrupted artifacts")
	}
}
