package synk

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

type mappingClient struct {
	incompleteFolderClient
	mu         sync.Mutex
	folders    map[string]icloud.Folder
	contents   map[string]string
	uploads    []string
	fail       string
	onDownload func()
}

func mappingFolder(id, name string) icloud.Item {
	return icloud.Item{DrivewsID: id, DocwsID: id, Zone: "zone", Name: name, Type: icloud.TypeFolder, Etag: "dir"}
}
func newMappingFixture(t *testing.T) (*Engine, *mappingClient) {
	t.Helper()
	cfg := config.Defaults()
	cfg.TargetDir = t.TempDir()
	cfg.SyncPhotos = false
	cfg.Workers = 1
	cfg.DriveSubfolders = []config.DriveMapping{{Remote: "ExampleBackups", Local: "backups"}, {Remote: "ExampleApp/widgets-data", Local: "widgets"}}
	root := mappingFolder(icloud.RootFolderID, "")
	a := mappingFolder("a", "ExampleBackups")
	app := mappingFolder("app", "ExampleApp")
	app.Type = icloud.TypeAppLibrary
	b := mappingFolder("b", "widgets-data")
	cl := &mappingClient{folders: map[string]icloud.Folder{root.DrivewsID: {Item: root, Items: []icloud.Item{a, app}}, "a": {Item: a}, "app": {Item: app, Items: []icloud.Item{b}}, "b": {Item: b}}, contents: map[string]string{}}
	for _, id := range []string{"a", "b"} {
		cl.addFile(id, "same.txt", id+" content")
	}
	st, err := LoadState(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(cfg, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	e.cl = cl
	return e, cl
}
func (c *mappingClient) addFile(parent, name, content string) {
	id := parent + "/" + name
	it := icloud.Item{DrivewsID: id, DocwsID: id, Zone: "zone", Name: name, Type: icloud.TypeFile, Size: int64(len(content)), Etag: fmt.Sprint("etag-", content), DateModified: time.Now().UTC().Add(-time.Hour)}
	f := c.folders[parent]
	for i, old := range f.Items {
		if old.FullName() == name {
			f.Items = append(f.Items[:i], f.Items[i+1:]...)
			break
		}
	}
	f.Items = append(f.Items, it)
	c.folders[parent] = f
	c.contents[id] = content
}
func (c *mappingClient) ListFolders(ctx context.Context, ids []string) ([]icloud.Folder, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []icloud.Folder
	for _, id := range ids {
		if id == c.fail {
			return nil, fmt.Errorf("inaccessible")
		}
		if f, ok := c.folders[id]; ok {
			f.Items = append([]icloud.Item(nil), f.Items...)
			out = append(out, f)
		}
	}
	return out, nil
}
func (c *mappingClient) Download(ctx context.Context, it icloud.Item, w io.Writer) (int64, error) {
	c.mu.Lock()
	data := c.contents[it.DrivewsID]
	hook := c.onDownload
	c.onDownload = nil
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	n, err := io.WriteString(w, data)
	return int64(n), err
}
func (c *mappingClient) Upload(ctx context.Context, zone, parent, name string, size int64, mtime time.Time, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uploads = append(c.uploads, parent+"/"+name)
	c.addFile(parent, name, string(data))
	return nil
}
func TestMappingsLifecycle(t *testing.T) {
	e, cl := newMappingFixture(t)
	ctx := context.Background()
	stats, err := e.SyncDrive(ctx)
	if err != nil || stats.Downloaded != 2 || stats.Errors != 0 {
		t.Fatalf("first: %+v %v", stats, err)
	}
	for _, m := range e.cfg.DriveSubfolders {
		data, err := os.ReadFile(e.localPath(m.Local + "/same.txt"))
		if err != nil || !strings.HasPrefix(string(data), map[string]string{"backups": "a", "widgets": "b"}[m.Local]) {
			t.Fatalf("wrong download: %q %v", data, err)
		}
	}
	st, err := LoadState(e.cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(st.Drive(), e.st.Drive()) {
		t.Fatal("reload differs")
	}
	e.st = st
	stats, err = e.SyncDrive(ctx)
	if err != nil || !stats.Empty() {
		t.Fatalf("repeat: %+v %v", stats, err)
	}
	for _, local := range []string{"backups", "widgets"} {
		if err := os.WriteFile(e.localPath(local+"/new.txt"), []byte(local), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stats, err = e.SyncDrive(ctx)
	if err != nil || stats.Uploaded != 2 || stats.Errors != 0 {
		t.Fatalf("uploads: %+v %v", stats, err)
	}
	if !reflect.DeepEqual(cl.uploads, []string{"a/new.txt", "b/new.txt"}) {
		t.Fatalf("wrong upload parents: %v", cl.uploads)
	}
	report, err := VerifyDrive(ctx, e.cfg, cl)
	if err != nil || !report.Complete() || report.Identical != 4 {
		t.Fatalf("verify: %+v %v", report, err)
	}
	stats, err = e.SyncDrive(ctx)
	if err != nil || !stats.Empty() {
		t.Fatalf("repeat uploads: %+v %v", stats, err)
	}
}
func TestMappingsDiscoveryAtomic(t *testing.T) {
	for _, failure := range []string{"missing", "inaccessible"} {
		t.Run(failure, func(t *testing.T) {
			e, cl := newMappingFixture(t)
			if failure == "missing" {
				delete(cl.folders, "b")
			} else {
				cl.fail = "b"
			}
			if _, err := e.SyncDrive(context.Background()); err == nil {
				t.Fatal("accepted incomplete inventory")
			}
			if _, err := os.Stat(e.localPath("backups/same.txt")); !os.IsNotExist(err) {
				t.Fatal("partial download applied")
			}
			if len(e.st.Drive()) != 0 || len(cl.uploads) != 0 {
				t.Fatal("partial state or upload")
			}
		})
	}
}
func TestMappingBindings(t *testing.T) {
	e, _ := newMappingFixture(t)
	if _, err := e.SyncDrive(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(e.cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.DriveSubfolders[0].Remote = "Different"
	if _, err := e.SyncDrive(context.Background()); err == nil {
		t.Fatal("accepted remap")
	}
	e.cfg.DriveSubfolders = nil
	if _, err := e.SyncDrive(context.Background()); err == nil {
		t.Fatal("accepted mapped state in legacy mode")
	}
	after, _ := os.ReadFile(e.cfg.StatePath())
	if string(before) != string(after) {
		t.Fatal("state changed")
	}
	e, _ = newMappingFixture(t)
	e.st.PutDrive("legacy.txt", Entry{Etag: "legacy"})
	e.st.Save()
	if _, err := e.SyncDrive(context.Background()); err == nil {
		t.Fatal("legacy state silently reinterpreted")
	}
}
func TestMappingsDryRun(t *testing.T) {
	e, _ := newMappingFixture(t)
	e.dryRun = true
	var output strings.Builder
	e.log = slog.New(slog.NewTextHandler(&output, nil))
	stats, err := e.SyncDrive(context.Background())
	if err != nil || stats.Skipped != 2 {
		t.Fatalf("%+v %v", stats, err)
	}
	for _, key := range []string{"backups/same.txt", "widgets/same.txt"} {
		if !strings.Contains(output.String(), key) {
			t.Fatal("mapping not in dry-run output")
		}
	}
	if _, err := os.Stat(e.cfg.StatePath()); !os.IsNotExist(err) {
		t.Fatal("dry run wrote state")
	}
}
func TestMappedConflictAndSymlinkSafety(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		e, cl := newMappingFixture(t)
		ctx := context.Background()
		if _, err := e.SyncDrive(ctx); err != nil {
			t.Fatal(err)
		}
		p := e.localPath("backups/same.txt")
		if err := os.WriteFile(p, []byte("local edit"), 0600); err != nil {
			t.Fatal(err)
		}
		cl.addFile("a", "same.txt", "remote edit")
		f := cl.folders["a"]
		f.Items[0].DateModified = time.Now().Add(time.Hour)
		cl.folders["a"] = f
		stats, err := e.SyncDrive(ctx)
		if err != nil || stats.Conflicts != 1 || stats.Errors != 0 {
			t.Fatalf("%+v %v", stats, err)
		}
		matches, _ := filepath.Glob(e.localPath("backups/*.conflict-*.txt"))
		if len(matches) != 1 {
			t.Fatalf("conflict copy missing: %v", matches)
		}
		data, _ := os.ReadFile(matches[0])
		if string(data) != "local edit" {
			t.Fatal("wrong conflict data")
		}
		data, _ = os.ReadFile(e.localPath("widgets/same.txt"))
		if string(data) != "b content" {
			t.Fatal("other mapping changed")
		}
	})
	t.Run("preexisting symlink", func(t *testing.T) {
		e, _ := newMappingFixture(t)
		outside := t.TempDir()
		os.MkdirAll(e.cfg.DriveDir(), 0755)
		os.Symlink(outside, e.localPath("backups"))
		if _, err := e.SyncDrive(context.Background()); err == nil {
			t.Fatal("accepted symlink root")
		}
	})
	t.Run("symlink during download", func(t *testing.T) {
		e, cl := newMappingFixture(t)
		outside := t.TempDir()
		cl.onDownload = func() {
			os.Rename(e.localPath("backups"), e.localPath("old-backups"))
			os.Symlink(outside, e.localPath("backups"))
		}
		stats, err := e.SyncDrive(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if stats.Errors == 0 {
			t.Fatal("expected confined write failure")
		}
		entries, _ := os.ReadDir(outside)
		if len(entries) != 0 {
			t.Fatal("wrote outside root")
		}
	})
}

func (c *mappingClient) MoveToTrash(ctx context.Context, items []icloud.Item) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, it := range items {
		for id, f := range c.folders {
			for i, child := range f.Items {
				if child.DrivewsID == it.DrivewsID {
					f.Items = append(f.Items[:i], f.Items[i+1:]...)
					c.folders[id] = f
					break
				}
			}
		}
	}
	return nil
}
func TestMappingsDeletionPolicies(t *testing.T) {
	for _, protect := range []bool{true, false} {
		t.Run(fmt.Sprint(protect), func(t *testing.T) {
			e, cl := newMappingFixture(t)
			ctx := context.Background()
			if _, err := e.SyncDrive(ctx); err != nil {
				t.Fatal(err)
			}
			e.cfg.ProtectDeletions = protect
			os.Remove(e.localPath("backups/same.txt"))
			f := cl.folders["b"]
			f.Items = nil
			cl.folders["b"] = f
			stats, err := e.SyncDrive(ctx)
			if err != nil || stats.Errors != 0 {
				t.Fatalf("%+v %v", stats, err)
			}
			if protect {
				if stats.TrashedLoc != 0 || stats.TrashedRem != 0 || len(e.st.Drive()) != 2 {
					t.Fatal("deletion protection lost")
				}
			} else {
				if stats.TrashedLoc != 1 || stats.TrashedRem != 1 {
					t.Fatalf("wrong deletions: %+v", stats)
				}
				matches, _ := filepath.Glob(filepath.Join(e.cfg.TrashDir(), "*", "widgets", "same.txt"))
				if len(matches) != 1 {
					t.Fatal("wrong trash destination")
				}
			}
		})
	}
}
func TestMappingsRetentionAndProvenance(t *testing.T) {
	e, c := retentionFixture(t, 1)
	e.cfg.DriveSubfolders = []config.DriveMapping{{Remote: "ExampleBackups", Local: "backups"}, {Remote: "ExampleApp/widgets-data", Local: "widgets"}}
	if err := e.st.checkMappings(e.cfg, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		for _, p := range []string{"backups/same.txt", "widgets/same.txt"} {
			replaceForRetention(t, e, c, p, fmt.Sprint(p, " version ", i))
		}
		if err := e.cleanupBackups(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"backups/same.txt", "widgets/same.txt"} {
			aa := completedBackups(e, p)
			if len(aa) != 1 {
				t.Fatalf("%s: %d backups", p, len(aa))
			}
			parent, _ := e.lookupDir(strings.Split(p, "/")[0])
			if aa[0].ParentID != parent.DrivewsID {
				t.Fatal("wrong backup parent")
			}
		}
	}
	if err := e.st.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(e.cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Artifacts()) != 2 {
		t.Fatal("artifact reload lost mapping provenance")
	}
}
func TestMappingLegacyStateUnchanged(t *testing.T) {
	e, cl := newMappingFixture(t)
	e.cfg.DriveSubfolders = nil
	e.cfg.DriveSubfolder = "ExampleBackups"
	if _, err := e.SyncDrive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.st.Drive()["same.txt"]; !ok {
		t.Fatal("legacy keys changed")
	}
	if len(e.st.data.Mappings) != 0 {
		t.Fatal("legacy state gained mapping bindings")
	}
	st, err := LoadState(e.cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	e.st = st
	stats, err := e.SyncDrive(context.Background())
	if err != nil || !stats.Empty() {
		t.Fatalf("legacy reload: %+v %v", stats, err)
	}
	report, err := VerifyDrive(context.Background(), e.cfg, cl)
	if err != nil || !report.Complete() {
		t.Fatalf("legacy verification: %+v %v", report, err)
	}
}

func (c *mappingClient) CreateFolder(ctx context.Context, parent, name string) (icloud.Item, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.folders[parent]; !ok {
		return icloud.Item{}, fmt.Errorf("unknown parent %s", parent)
	}
	id := parent + "/" + name
	item := mappingFolder(id, name)
	f := c.folders[parent]
	f.Items = append(f.Items, item)
	c.folders[parent] = f
	c.folders[id] = icloud.Folder{Item: item}
	return item, nil
}
func TestMappingsNestedDestinationsAndFolders(t *testing.T) {
	e, cl := newMappingFixture(t)
	e.cfg.DriveSubfolders[0].Local = "group/backups"
	e.cfg.DriveSubfolders[1].Local = "group/widgets"
	p := e.localPath("group/widgets/new-dir/upload.txt")
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("new nested content"), 0600)
	stats, err := e.SyncDrive(context.Background())
	if err != nil || stats.Errors != 0 || stats.Uploaded != 1 || stats.Downloaded != 2 {
		t.Fatalf("%+v %v", stats, err)
	}
	if !reflect.DeepEqual(cl.uploads, []string{"b/new-dir/upload.txt"}) {
		t.Fatalf("wrong nested upload: %v", cl.uploads)
	}
	if _, ok := cl.folders["group"]; ok {
		t.Fatal("local mapping ancestor was created remotely")
	}
	stats, err = e.SyncDrive(context.Background())
	if err != nil || !stats.Empty() {
		t.Fatalf("repeat nested sync: %+v %v", stats, err)
	}
	report, err := VerifyDrive(context.Background(), e.cfg, cl)
	if err != nil || !report.Complete() {
		t.Fatalf("verify nested: %+v %v", report, err)
	}
}
func TestMappingsMissingRootAndReordering(t *testing.T) {
	e, cl := newMappingFixture(t)
	e.cfg.DriveSubfolders[1].Remote = "ExampleApp/missing"
	if _, err := e.SyncDrive(context.Background()); err == nil {
		t.Fatal("accepted missing root")
	}
	if len(e.st.Drive()) != 0 {
		t.Fatal("applied partial plan")
	}
	if len(cl.folders) != 4 {
		t.Fatal("created missing remote root")
	}
	e.cfg.DriveSubfolders[1].Remote = "ExampleApp/widgets-data"
	if _, err := e.SyncDrive(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.cfg.DriveSubfolders[0], e.cfg.DriveSubfolders[1] = e.cfg.DriveSubfolders[1], e.cfg.DriveSubfolders[0]
	stats, err := e.SyncDrive(context.Background())
	if err != nil || !stats.Empty() {
		t.Fatalf("reorder changed behavior: %+v %v", stats, err)
	}
}

func TestMappingsVerifyBackupProvenance(t *testing.T) {
	e, cl := newMappingFixture(t)
	ctx := context.Background()
	if _, err := e.SyncDrive(ctx); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"a", "backups"}, {"b", "widgets"}} {
		name := replacementSiblingName("same.txt", "backup", "20261014T140000.123456789")
		cl.addFile(pair[0], name, "old")
		f := cl.folders[pair[0]]
		item := f.Items[len(f.Items)-1]
		e.st.putArtifact(ReplacementArtifact{Version: 1, Original: pair[1] + "/same.txt", ParentID: pair[0], Kind: "backup", Item: item, Complete: true})
	}
	if err := e.st.Save(); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyDrive(ctx, e.cfg, cl)
	if err != nil || !report.Complete() || len(report.SkippedInternal) != 2 {
		t.Fatalf("mapped artifacts: %+v %v", report, err)
	}
}
func TestMappingsTransitionPreservesPhotoState(t *testing.T) {
	e, _ := newMappingFixture(t)
	e.st.PutPhoto("photo-id", PhotoEntry{Path: "2026/01/example.jpg", Size: 10})
	if err := e.st.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(e.cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	e.st = st
	if _, err := e.SyncDrive(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err = LoadState(e.cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if photo, ok := st.Photo("photo-id"); !ok || photo.Path != "2026/01/example.jpg" {
		t.Fatal("empty legacy Drive transition lost photo state")
	}
	if st.data.Version != 1 || len(st.data.Mappings) != 2 {
		t.Fatal("unexpected state representation")
	}
}
