package synk

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

type trackingVerifyClient struct {
	driveVerifyClient
	downloads []string
}

func (c *trackingVerifyClient) Download(ctx context.Context, item icloud.Item, w io.Writer) (int64, error) {
	c.downloads = append(c.downloads, item.DrivewsID)
	return c.driveVerifyClient.Download(ctx, item, w)
}

func TestVerifyRetainedBackupsWithoutProvenance(t *testing.T) {
	for _, layout := range []string{"multi-app-container", "legacy-subfolder"} {
		for _, difference := range []string{"none", "missing-local", "missing-remote", "modified", "lookalike", "local-backup-counterpart", "matching-local-backup"} {
			t.Run(layout+"/"+difference, func(t *testing.T) {
				e, cl := newMappingFixture(t)
				locals := []string{"backups", "widgets"}
				parents := []string{"a", "b"}
				if layout == "legacy-subfolder" {
					e.cfg.DriveSubfolders = nil
					e.cfg.DriveSubfolder = "ExampleBackups"
					locals = []string{""}
					parents = []string{"a"}
				}
				backupIDs := map[string]bool{}
				for i, local := range locals {
					root := filepath.Join(e.cfg.DriveDir(), local)
					if err := os.MkdirAll(root, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(root, "same.txt"), []byte(cl.contents[parents[i]+"/same.txt"]), 0600); err != nil {
						t.Fatal(err)
					}
					for _, original := range []string{"report.release.v2.json", "archive.tar.gz"} {
						name := replacementSiblingName(original, "backup", "20261030T124223.559975819")
						cl.addFile(parents[i], name, "old")
						backupIDs[parents[i]+"/"+name] = true
						// Exercise the API's separate Name/Extension representation as well.
						f := cl.folders[parents[i]]
						item := &f.Items[len(f.Items)-1]
						ext := filepath.Ext(name)
						item.Name = strings.TrimSuffix(name, ext)
						item.Extension = strings.TrimPrefix(ext, ".")
						cl.folders[parents[i]] = f
					}
				}
				local := locals[len(locals)-1]
				parent := parents[len(parents)-1]
				p := filepath.Join(e.cfg.DriveDir(), local, "same.txt")
				switch difference {
				case "missing-local":
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
				case "missing-remote":
					f := cl.folders[parent]
					f.Items = f.Items[1:]
					cl.folders[parent] = f
				case "modified":
					if err := os.WriteFile(p, []byte("X content"), 0600); err != nil {
						t.Fatal(err)
					}
				case "lookalike":
					cl.addFile(parent, "report.icloud-synk-backup-my-notes.json", "user data")
				case "local-backup-counterpart", "matching-local-backup":
					name := replacementSiblingName("report.release.v2.json", "backup", "20261030T124223.559975819")
					content := "NEW"
					if difference == "matching-local-backup" {
						content = "old"
					}
					if err := os.WriteFile(filepath.Join(filepath.Dir(p), name), []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
					delete(backupIDs, parent+"/"+name)
				}
				tracker := &trackingVerifyClient{driveVerifyClient: cl}
				report, err := VerifyDrive(context.Background(), e.cfg, tracker)
				if err != nil {
					t.Fatal(err)
				}
				if report.Complete() != (difference == "none" || difference == "matching-local-backup") {
					t.Fatalf("wrong completion: %+v", report)
				}
				if difference != "none" && difference != "matching-local-backup" && len(report.Differences) == 0 {
					t.Fatalf("real difference hidden: %+v", report)
				}
				if len(report.SkippedInternal) != len(backupIDs) {
					t.Fatalf("wrong skipped count: %+v", report)
				}
				for _, skipped := range report.SkippedInternal {
					if !strings.Contains(skipped, "provenance unavailable") {
						t.Fatalf("unproven artifact not labeled: %s", skipped)
					}
				}
				for _, id := range tracker.downloads {
					if backupIDs[id] {
						t.Fatalf("downloaded remote-only backup: %s", id)
					}
				}
				wantLocal, wantRemote, wantIdentical := len(locals), len(locals), len(locals)
				switch difference {
				case "missing-local":
					wantLocal--
					wantIdentical--
				case "missing-remote":
					wantRemote--
					wantIdentical--
				case "modified":
					wantIdentical--
				case "lookalike":
					wantRemote++
				case "matching-local-backup":
					wantIdentical++
					fallthrough
				case "local-backup-counterpart":
					wantLocal++
					wantRemote++
				}
				if report.LocalFiles != wantLocal || report.RemoteFiles != wantRemote || report.Identical != wantIdentical {
					t.Fatalf("wrong comparison counters: %+v; want %d/%d/%d", report, wantLocal, wantRemote, wantIdentical)
				}
				if _, err := os.Stat(e.cfg.StatePath()); !os.IsNotExist(err) {
					t.Fatal("verification created synchronization state")
				}
			})
		}
	}
}

// Filename classification cannot override a journal left by an interrupted
// replacement, even when the remote item now has a different identity.
func TestVerifyHistoricalBackupCannotBypassPendingProvenance(t *testing.T) {
	e, cl := newMappingFixture(t)
	for _, m := range e.cfg.DriveSubfolders {
		if err := os.MkdirAll(e.localPath(m.Local), 0700); err != nil {
			t.Fatal(err)
		}
	}
	name := replacementSiblingName("report.v2.json", "backup", "20261030T124223.559975819")
	cl.addFile("a", name, "old")
	f := cl.folders["a"]
	item := f.Items[len(f.Items)-1]
	if err := e.st.checkMappings(e.cfg, true); err != nil {
		t.Fatal(err)
	}
	e.st.putArtifact(ReplacementArtifact{Version: 1, Original: "backups/report.v2.json", ParentID: "a", Kind: "backup", Item: item})
	if err := e.st.Save(); err != nil {
		t.Fatal(err)
	}
	f.Items[len(f.Items)-1].DrivewsID = "new-id"
	cl.folders["a"] = f
	report, err := VerifyDrive(context.Background(), e.cfg, cl)
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete() || len(report.SkippedInternal) != 0 {
		t.Fatalf("pending provenance bypassed: %+v", report)
	}
}
