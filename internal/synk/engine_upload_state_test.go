package synk

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

// All remote operations in these tests are in memory. Embedding supplies only
// the unused driveClient methods; the replacement flow is implemented below.
type uploadStateClient struct {
	incompleteFolderClient
	parent                      icloud.Item
	items                       map[string]icloud.Item
	contents                    map[string]string
	uploads, downloads, renames int
	lists                       int
	afterUpload                 func()
	beforeDownload              func(icloud.Item)
	refresh                     func() ([]icloud.Folder, error)
}

func newUploadStateFixture(t *testing.T) (*Engine, *uploadStateClient) {
	t.Helper()
	cfg := config.Defaults()
	cfg.TargetDir, cfg.Conflict, cfg.ProtectDeletions = t.TempDir(), "both", true
	st, err := LoadState(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(cfg, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	if err := e.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	parent := icloud.Item{DrivewsID: "root", DocwsID: "root-doc", Zone: icloud.DefaultZone, Type: icloud.TypeFolder}
	cl := &uploadStateClient{parent: parent, items: map[string]icloud.Item{}, contents: map[string]string{}}
	e.cl = cl
	e.putDir("", parent)
	if err := os.WriteFile(e.localPath("new.json"), []byte("new content"), 0600); err != nil {
		t.Fatal(err)
	}
	return e, cl
}

func TestInitialUploadRequiresVerifiedMetadata(t *testing.T) {
	for _, mode := range []string{"stable", "list-error", "missing-folder", "missing-item", "wrong-size", "wrong-content", "empty-etag", "empty-id", "empty-doc-id", "empty-zone", "directory"} {
		t.Run(mode, func(t *testing.T) {
			e, cl := newUploadStateFixture(t)
			// A changed same-size sibling must never become synchronized as a
			// side effect of completing metadata for the new file.
			sibling := remoteFile("sibling.json", "baseline", 3, time.Time{})
			e.st.PutDrive("sibling.json", Entry{Size: 3, Etag: sibling.Etag, DrivewsID: sibling.DrivewsID, DocwsID: sibling.DocwsID, SHA256: hashString("old")})
			before := e.st.Drive()
			sibling.Etag = "remote-edit"
			cl.items[sibling.DrivewsID], cl.contents[sibling.DrivewsID] = sibling, "NEW"
			cl.afterUpload = func() {
				switch mode {
				case "list-error":
					cl.refresh = func() ([]icloud.Folder, error) { return nil, fmt.Errorf("mock listing failure") }
				case "missing-folder":
					cl.refresh = func() ([]icloud.Folder, error) { return nil, nil }
				case "missing-item":
					cl.refresh = func() ([]icloud.Folder, error) {
						return []icloud.Folder{{Item: cl.parent, Items: []icloud.Item{sibling}}}, nil
					}
				default:
					it := cl.items["new-drive-id"]
					switch mode {
					case "wrong-size":
						it.Size++
					case "wrong-content":
						cl.contents[it.DrivewsID] = "REMOTE EDIT"
					case "empty-etag":
						it.Etag = ""
					case "empty-id":
						it.DrivewsID = ""
					case "empty-doc-id":
						it.DocwsID = ""
					case "empty-zone":
						it.Zone = ""
					case "directory":
						it.Type = icloud.TypeFolder
					}
					cl.items["new-drive-id"] = it
				}
			}
			stats := e.transfer(context.Background(), Op{Kind: OpUpload, Path: "new.json"}, "test")
			if mode == "stable" {
				if stats.Errors != 0 || stats.Uploaded != 1 {
					t.Fatalf("upload: %+v", stats)
				}
			} else {
				if stats.Errors != 1 || stats.Uploaded != 0 {
					t.Fatalf("incomplete creation was reported successful: %+v", stats)
				}
				if !reflect.DeepEqual(e.st.Drive(), before) {
					t.Fatal("failed confirmation advanced state")
				}
			}
			if e.st.Drive()["sibling.json"] != before["sibling.json"] {
				t.Fatal("unrelated sibling baseline overwritten")
			}
			if !remoteChanged(sibling, before["sibling.json"], true) {
				t.Fatal("genuine sibling edit hidden")
			}
			if cl.uploads != 1 || len(cl.items) != 2 || cl.renames != 0 {
				t.Fatal("remote versions not retained")
			}
			if err := e.st.Save(); err != nil {
				t.Fatal(err)
			}
			reloaded, err := LoadState(e.cfg.StatePath())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reloaded.Drive(), e.st.Drive()) {
				t.Fatal("state did not survive restart")
			}
			if mode == "stable" {
				local, _, _, err := e.scanner.Scan()
				if err != nil {
					t.Fatal(err)
				}
				ops := Reconcile(local, map[string]icloud.Item{"new.json": cl.items["new-drive-id"]}, reloaded.Drive(), Policy{Conflict: "both", ProtectDeletions: true})
				if op := findOp(t, ops, "new.json"); op.Kind != OpRecord {
					t.Fatalf("creation did not converge: %+v", op)
				}
			}
		})
	}
}

func TestUploadSourceMutationDoesNotAdvanceState(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint("replacement=", replacement), func(t *testing.T) {
			e, cl := newUploadStateFixture(t)
			op := Op{Kind: OpUpload, Path: "new.json"}
			if replacement {
				op.Remote = remoteFile("new.json", "old-etag", 3, time.Time{})
				cl.items[op.Remote.DrivewsID], cl.contents[op.Remote.DrivewsID] = op.Remote, "old"
				e.st.PutDrive(op.Path, Entry{Size: 3, Etag: op.Remote.Etag, DrivewsID: op.Remote.DrivewsID})
			}
			before := e.st.Drive()
			cl.afterUpload = func() {
				if err := os.WriteFile(e.localPath(op.Path), []byte("local edit!"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := e.upload(context.Background(), op, "test")
			if err == nil || !strings.Contains(err.Error(), "changed during upload") {
				t.Fatalf("source mutation not reported: %v", err)
			}
			if !reflect.DeepEqual(before, e.st.Drive()) {
				t.Fatal("changed source recorded as synchronized")
			}
			if got := cl.contents["new-drive-id"]; got != "new content" {
				t.Fatalf("uploaded snapshot changed: %q", got)
			}
			if replacement && len(cl.items) != 2 {
				t.Fatal("replacement backup lost")
			}
		})
	}
}

func TestReplacementVerificationFailurePreservesStateAndVersions(t *testing.T) {
	for _, stage := range []string{"temporary", "final"} {
		t.Run(stage, func(t *testing.T) {
			e, cl := newUploadStateFixture(t)
			old := remoteFile("new.json", "old-etag", 3, time.Time{})
			cl.items[old.DrivewsID], cl.contents[old.DrivewsID] = old, "old"
			e.st.PutDrive("new.json", Entry{Size: 3, Etag: old.Etag, DrivewsID: old.DrivewsID, SHA256: hashString("old")})
			before := e.st.Drive()
			cl.beforeDownload = func(it icloud.Item) {
				if it.DrivewsID == "new-drive-id" && ((stage == "temporary" && isReplacementSiblingName(it.FullName())) || (stage == "final" && it.FullName() == "new.json")) {
					cl.contents[it.DrivewsID] = "bad content"
				}
			}
			_, err := e.upload(context.Background(), Op{Kind: OpUpload, Path: "new.json", Remote: old, Conflict: true, Winner: WinnerLocal}, "test")
			if err == nil {
				t.Fatal("verification failure accepted")
			}
			if !reflect.DeepEqual(e.st.Drive(), before) {
				t.Fatal("failed replacement advanced state")
			}
			if len(cl.items) != 2 || cl.contents[old.DrivewsID] != "old" || cl.items[old.DrivewsID].FullName() != "new.json" {
				t.Fatal("old remote version not preserved/restored")
			}
			if stage == "final" && !strings.Contains(cl.items["new-drive-id"].FullName(), ".icloud-synk-failed-") {
				t.Fatal("failed new version not retained")
			}
			b, err := os.ReadFile(conflictPath(e.localPath("new.json"), "test"))
			if err != nil || string(b) != "old" {
				t.Fatalf("conflict copy lost: %q, %v", b, err)
			}
		})
	}
}

func TestReplacementDoesNotAcceptUnrelatedSiblingEdit(t *testing.T) {
	e, cl := newUploadStateFixture(t)
	old := remoteFile("new.json", "old-etag", 3, time.Time{})
	sibling := remoteFile("sibling.json", "baseline", 3, time.Time{})
	cl.items[old.DrivewsID], cl.contents[old.DrivewsID] = old, "old"
	cl.items[sibling.DrivewsID], cl.contents[sibling.DrivewsID] = sibling, "old"
	e.st.PutDrive("sibling.json", Entry{Size: 3, Etag: sibling.Etag, DrivewsID: sibling.DrivewsID, DocwsID: sibling.DocwsID})
	before := e.st.Drive()["sibling.json"]
	cl.afterUpload = func() {
		sibling.Etag = "remote-edit"
		cl.items[sibling.DrivewsID], cl.contents[sibling.DrivewsID] = sibling, "NEW"
	}
	if _, err := e.upload(context.Background(), Op{Kind: OpUpload, Path: "new.json", Remote: old}, "test"); err != nil {
		t.Fatal(err)
	}
	e.apply(context.Background(), nil)
	if e.st.Drive()["sibling.json"] != before {
		t.Fatal("unrelated sibling edit accepted without transfer")
	}
	ops := Reconcile(map[string]LocalFile{"sibling.json": file("sibling.json", 3, time.Time{})}, map[string]icloud.Item{"sibling.json": sibling}, e.st.Drive(), Policy{Conflict: "both", ProtectDeletions: true})
	if op := findOp(t, ops, "sibling.json"); op.Kind != OpDownload || op.Conflict {
		t.Fatalf("sibling edit hidden: %+v", op)
	}
}

func TestReplacementAbortsIfRemoteChangesAfterPlanning(t *testing.T) {
	for _, mode := range []string{"etag", "identity", "size"} {
		t.Run(mode, func(t *testing.T) {
			e, cl := newUploadStateFixture(t)
			planned := remoteFile("new.json", "baseline", 3, time.Time{})
			current := planned
			switch mode {
			case "etag":
				current.Etag = "edited"
			case "identity":
				current.DrivewsID = "other-id"
			case "size":
				current.Size++
			}
			cl.items[current.DrivewsID], cl.contents[current.DrivewsID] = current, "edit"
			_, err := e.upload(context.Background(), Op{Kind: OpUpload, Path: "new.json", Remote: planned}, "test")
			if err == nil || !strings.Contains(err.Error(), "changed since reconciliation") {
				t.Fatalf("remote change not reported: %v", err)
			}
			if cl.uploads != 0 || cl.renames != 0 || len(e.st.Drive()) != 0 || cl.contents[current.DrivewsID] != "edit" {
				t.Fatal("changed remote file was modified or recorded")
			}
		})
	}
}

func (c *uploadStateClient) ListFolders(_ context.Context, _ []string) ([]icloud.Folder, error) {
	c.lists++
	if c.refresh != nil {
		return c.refresh()
	}
	var items []icloud.Item
	for _, it := range c.items {
		items = append(items, it)
	}
	return []icloud.Folder{{Item: c.parent, Items: items}}, nil
}

func (c *uploadStateClient) Upload(_ context.Context, _, _, name string, size int64, mt time.Time, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.uploads++
	it := remoteFile(name, "uploaded", size, mt)
	it.DrivewsID, it.DocwsID = "new-drive-id", "new-doc-id"
	c.items[it.DrivewsID], c.contents[it.DrivewsID] = it, string(b)
	if c.afterUpload != nil {
		c.afterUpload()
	}
	return nil
}

func (c *uploadStateClient) Download(_ context.Context, it icloud.Item, w io.Writer) (int64, error) {
	c.downloads++
	if c.beforeDownload != nil {
		c.beforeDownload(it)
	}
	n, err := io.WriteString(w, c.contents[it.DrivewsID])
	return int64(n), err
}

func (c *uploadStateClient) Rename(_ context.Context, it icloud.Item, name string) error {
	c.renames++
	it.Name, it.Extension, it.Etag = name, "", fmt.Sprintf("rename-%d", c.renames)
	c.items[it.DrivewsID] = it
	return nil
}

func TestUploadReplacementStateLifecycle(t *testing.T) {
	for _, mode := range []string{"stable", "refresh-error", "missing-folder", "missing-item", "stale-old-item", "later-etag-change", "size-only-change", "identity-only-change", "doc-id-change", "zone-change", "genuine-content-change"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			cfg := config.Defaults()
			cfg.TargetDir, cfg.Conflict, cfg.ProtectDeletions = t.TempDir(), "both", true
			st, err := LoadState(cfg.StatePath())
			if err != nil {
				t.Fatal(err)
			}
			e := NewEngine(cfg, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
			if err := e.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			const name = "linux-test.json"
			oldTime := time.Date(2026, 10, 29, 16, 0, 0, 0, time.UTC)
			old := remoteFile(name, "old-etag", 3, oldTime)
			parent := icloud.Item{DrivewsID: "test-root", DocwsID: "test-root-doc", Zone: icloud.DefaultZone, Type: icloud.TypeFolder}
			cl := &uploadStateClient{parent: parent, items: map[string]icloud.Item{old.DrivewsID: old}, contents: map[string]string{old.DrivewsID: "old"}}
			e.cl = cl
			e.putDir("", parent)
			if err := os.WriteFile(e.localPath(name), []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(e.localPath(name), oldTime, oldTime); err != nil {
				t.Fatal(err)
			}
			st.PutDrive(name, Entry{Size: 3, LocalMTime: oldTime, SHA256: hashString("old"), Etag: old.Etag, DrivewsID: old.DrivewsID, DocwsID: old.DocwsID, Zone: old.Zone, RemoteMTime: oldTime})
			policy := Policy{Conflict: "both", ProtectDeletions: true}
			plan := func(remote icloud.Item) []Op {
				t.Helper()
				local, _, _, err := e.scanner.Scan()
				if err != nil {
					t.Fatal(err)
				}
				return Reconcile(local, map[string]icloud.Item{name: remote}, e.st.Drive(), policy)
			}
			if op := findOp(t, plan(old), name); op.Kind != OpRecord {
				t.Fatalf("initial plan: %+v", op)
			}
			if err := os.WriteFile(e.localPath(name), []byte("new content"), 0600); err != nil {
				t.Fatal(err)
			}
			mt := oldTime.Add(time.Hour)
			if err := os.Chtimes(e.localPath(name), mt, mt); err != nil {
				t.Fatal(err)
			}
			op := findOp(t, plan(old), name)
			if op.Kind != OpUpload || op.Conflict {
				t.Fatalf("local edit: %+v", op)
			}
			if _, err := e.upload(ctx, op, "test-pass"); err != nil {
				t.Fatal(err)
			}
			target := cl.items["new-drive-id"]
			if target.FullName() != name || cl.contents[target.DrivewsID] != "new content" {
				t.Fatal("replacement not confirmed")
			}
			if cl.uploads != 1 || cl.renames != 2 || cl.downloads != 2 {
				t.Fatalf("unexpected replacement operations: %+v", cl)
			}
			if len(cl.items) != 2 {
				t.Fatal("expected target plus retained backup")
			}
			for id, it := range cl.items {
				if id != target.DrivewsID && !isReplacementSiblingName(it.FullName()) {
					t.Fatal("old version not retained as backup")
				}
			}
			switch mode {
			case "refresh-error":
				cl.refresh = func() ([]icloud.Folder, error) { return nil, fmt.Errorf("mock metadata unavailable") }
			case "missing-folder":
				cl.refresh = func() ([]icloud.Folder, error) { return nil, nil }
			case "missing-item":
				cl.refresh = func() ([]icloud.Folder, error) { return []icloud.Folder{{Item: parent}}, nil }
			case "stale-old-item":
				cl.refresh = func() ([]icloud.Folder, error) {
					return []icloud.Folder{{Item: parent, Items: []icloud.Item{old}}}, nil
				}
			}
			// No folder-wide refresh remains: even an empty application must
			// leave the verified baseline intact and make no metadata requests.
			lists := cl.lists
			e.apply(ctx, nil)
			if cl.lists != lists {
				t.Fatal("confirmed replacement was refreshed")
			}
			if err := st.Save(); err != nil {
				t.Fatal(err)
			}
			// Restart from the persisted test state to include serialization.
			e.st, err = LoadState(cfg.StatePath())
			if err != nil {
				t.Fatal(err)
			}
			saved := e.st.Drive()[name]
			if localChanged(file(name, 11, mt), saved, true) || saved.SHA256 != hashString("new content") {
				t.Fatalf("local baseline lost: %+v", saved)
			}
			if saved.DrivewsID != target.DrivewsID || saved.DocwsID != target.DocwsID || saved.Etag != target.Etag || saved.Zone != target.Zone || !saved.RemoteMTime.Equal(target.ModTime()) || saved.Size != target.Size {
				t.Fatalf("verified metadata lost: %+v", saved)
			}
			if mode == "later-etag-change" {
				target.Etag = "later-etag"
			}
			if mode == "size-only-change" {
				target.Size++
			}
			if mode == "identity-only-change" {
				target.DrivewsID = "other-id"
			}
			if mode == "doc-id-change" {
				target.DocwsID = "other-doc"
			}
			if mode == "zone-change" {
				target.Zone = "other-zone"
			}
			if mode == "genuine-content-change" {
				target.Etag = "edited-remotely"
				cl.items[target.DrivewsID] = target
				cl.contents[target.DrivewsID] = "remote edit" // same size
			}
			next := plan(target)
			want := OpRecord
			switch mode {
			case "later-etag-change", "size-only-change", "identity-only-change", "doc-id-change", "zone-change", "genuine-content-change":
				want = OpDownload
			}
			if got := findOp(t, next, name); got.Kind != want || got.Conflict {
				t.Fatalf("next pass: %+v, want %s without conflict", got, want)
			}
			if len(next) != 1 {
				t.Fatalf("unexpected local conflict file: %+v", next)
			}
			if mode == "genuine-content-change" {
				// Same-size remote content edits must still be downloaded, and
				// become conflicts if the local content also changes.
				local, _, _, err := e.scanner.Scan()
				if err != nil {
					t.Fatal(err)
				}
				l := local[name]
				l.Size++
				local[name] = l
				conflict := findOp(t, Reconcile(local, map[string]icloud.Item{name: target}, e.st.Drive(), policy), name)
				if !conflict.Conflict {
					t.Fatal("two-sided edit was not a conflict")
				}
				stats := e.apply(ctx, next)
				b, err := os.ReadFile(e.localPath(name))
				if err != nil || string(b) != "remote edit" || stats.Downloaded != 1 || stats.Errors != 0 {
					t.Fatalf("genuine remote edit not applied: %q, %+v, %v", b, stats, err)
				}
			}
			if want == OpRecord {
				stats := e.apply(ctx, next)
				if !stats.Empty() || stats.Conflicts != 0 || cl.uploads != 1 || cl.renames != 2 || cl.downloads != 2 {
					t.Fatalf("redundant work: %+v", stats)
				}
			}
		})
	}
}

func TestReplacementConflictClassificationEvidence(t *testing.T) {
	mt := time.Date(2026, 10, 29, 16, 0, 0, 0, time.UTC)
	base := Entry{Size: 3, LocalMTime: mt, Etag: "baseline", DrivewsID: remoteFile("test.json", "baseline", 3, mt).DrivewsID}
	for _, tc := range []struct {
		name         string
		change       func(*icloud.Item, *Entry)
		hasState     bool
		wantConflict bool
	}{
		{"only-local-edit", func(*icloud.Item, *Entry) {}, true, false},
		{"remote-etag-drift", func(r *icloud.Item, _ *Entry) { r.Etag = "changed" }, true, true},
		{"remote-size-drift", func(r *icloud.Item, _ *Entry) { r.Size++ }, true, true},
		{"unknown-etag", func(_ *icloud.Item, s *Entry) { s.Etag = "" }, true, true},
		{"missing-state", func(*icloud.Item, *Entry) {}, false, true},
		{"identity-only-change", func(r *icloud.Item, _ *Entry) { r.DrivewsID, r.DocwsID = "different", "different" }, true, true},
		{"remote-time-only-change-ignored", func(r *icloud.Item, _ *Entry) { r.DateModified = mt.Add(time.Minute) }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s := remoteFile("test.json", "baseline", 3, mt), base
			tc.change(&r, &s)
			state := map[string]Entry{}
			if tc.hasState {
				state["test.json"] = s
			}
			ops := Reconcile(map[string]LocalFile{"test.json": file("test.json", 11, mt.Add(time.Hour))}, map[string]icloud.Item{"test.json": r}, state, Policy{Conflict: "both", ProtectDeletions: true})
			op := findOp(t, ops, "test.json")
			if op.Kind != OpUpload || op.Conflict != tc.wantConflict {
				t.Fatalf("classification: %+v", op)
			}
		})
	}
}
