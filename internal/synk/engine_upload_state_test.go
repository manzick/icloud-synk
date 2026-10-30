package synk

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	refresh                     func() ([]icloud.Folder, error)
}

func (c *uploadStateClient) ListFolders(_ context.Context, _ []string) ([]icloud.Folder, error) {
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
	return nil
}

func (c *uploadStateClient) Download(_ context.Context, it icloud.Item, w io.Writer) (int64, error) {
	c.downloads++
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
	// The non-stable cases characterize current defects, rather than asserting
	// the desired fixed behavior. A future fix must update those expectations.
	for _, mode := range []string{"stable", "refresh-error", "missing-folder", "missing-item", "stale-old-item", "later-etag-change", "size-only-change"} {
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
			err = e.refreshEtags(ctx)
			if (err != nil) != (mode == "refresh-error") {
				t.Fatalf("refresh error: %v", err)
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
			if mode == "stale-old-item" && saved.DrivewsID != old.DrivewsID {
				t.Fatal("expected characterization: refresh overwrites confirmed identity")
			}
			if mode == "later-etag-change" {
				target.Etag = "later-etag"
			}
			if mode == "size-only-change" {
				target.Size++
			}
			next := plan(target)
			want := OpDownload
			if mode == "stable" {
				want = OpRecord
			}
			if got := findOp(t, next, name); got.Kind != want || got.Conflict {
				t.Fatalf("next pass: %+v, want %s without conflict", got, want)
			}
			if len(next) != 1 {
				t.Fatalf("unexpected local conflict file: %+v", next)
			}
			if mode == "stable" {
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
	base := Entry{Size: 3, LocalMTime: mt, Etag: "baseline", DrivewsID: "old-id"}
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
		{"identity-only-change-ignored", func(r *icloud.Item, _ *Entry) { r.DrivewsID, r.DocwsID = "different", "different" }, true, false},
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
