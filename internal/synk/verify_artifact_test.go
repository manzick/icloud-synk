package synk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

func TestVerifyDriveArtifactOwnershipAndNormalDifferences(t *testing.T) {
	for _, mode := range []string{"identical", "backup", "multiple-backups", "ordinary", "missing-remote", "content-mismatch", "historical-backup", "lookalike", "pending-backup", "temporary", "unknown-temporary", "failed", "changed-etag", "changed-id", "changed-parent", "wrong-kind", "wrong-original", "local-counterpart", "pending-local-counterpart"} {
		t.Run(mode, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.TargetDir = t.TempDir()
			if err := os.MkdirAll(cfg.DriveDir(), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.DriveDir(), "hello.txt"), []byte("hello"), 0600); err != nil {
				t.Fatal(err)
			}
			st, err := LoadState(cfg.StatePath())
			if err != nil {
				t.Fatal(err)
			}
			normal := remoteFile("hello.txt", "normal", 5, time.Time{})
			items := []icloud.Item{normal}
			data := map[string][]byte{normal.DocwsID: []byte("hello")}
			wantSkipped, wantComplete := 0, mode == "identical" || mode == "backup" || mode == "multiple-backups" || mode == "historical-backup"
			switch mode {
			case "identical":
			case "missing-remote":
				items = nil
			case "content-mismatch":
				data[normal.DocwsID] = []byte("HELLO")
			default:
				n := 1
				if mode == "multiple-backups" {
					n = 3
				}
				for i := 0; i < n; i++ {
					kind := "backup"
					if mode == "temporary" || mode == "unknown-temporary" || mode == "pending-local-counterpart" {
						kind = "new"
					}
					if mode == "failed" {
						kind = "failed"
					}
					name := replacementSiblingName("hello.txt", kind, fmt.Sprintf("20261030T12422%d.559975819", i))
					if mode == "ordinary" {
						name = "user.txt"
					}
					if mode == "lookalike" {
						name = "hello.icloud-synk-backup-my-notes.txt"
					}
					it := remoteFile(name, "artifact", 3, time.Time{})
					it.DrivewsID, it.DocwsID = fmt.Sprint("artifact-", i), fmt.Sprint("artifact-doc-", i)
					a := ReplacementArtifact{Version: 1, Original: "hello.txt", ParentID: icloud.RootFolderID, Kind: kind, Item: it, Created: time.Now().UTC(), Complete: kind == "backup" && mode != "pending-backup"}
					owned := mode != "historical-backup" && mode != "lookalike" && mode != "ordinary" && mode != "unknown-temporary"
					if owned {
						st.putArtifact(a)
					}
					switch mode {
					case "changed-etag":
						it.Etag = "user-edit"
					case "changed-id":
						it.DrivewsID = "user-id"
					case "changed-parent":
						a.ParentID = "different-folder"
						st.putArtifact(a)
					case "wrong-kind":
						a.Kind = "new"
						st.putArtifact(a)
					case "wrong-original":
						a.Original = "other.txt"
						st.putArtifact(a)
					case "local-counterpart", "pending-local-counterpart":
						if err := os.WriteFile(filepath.Join(cfg.DriveDir(), name), []byte("old"), 0600); err != nil {
							t.Fatal(err)
						}
						data[it.DocwsID] = []byte("old")
						wantComplete = mode == "local-counterpart"
					}
					if mode == "historical-backup" {
						wantSkipped++
					}
					items = append(items, it)
					if owned && mode != "changed-etag" && mode != "changed-id" && mode != "changed-parent" && mode != "wrong-kind" && mode != "wrong-original" && mode != "local-counterpart" && mode != "pending-local-counterpart" {
						wantSkipped++
					}
				}
			}
			if err := st.Save(); err != nil {
				t.Fatal(err)
			}
			stateBefore, err := os.ReadFile(cfg.StatePath())
			if err != nil { // No dirty state in the normal-file-only cases.
				if !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			cl := &verifyFakeClient{folders: map[string]icloud.Folder{icloud.RootFolderID: {Item: icloud.Item{DrivewsID: icloud.RootFolderID}, Items: items}}, data: data}
			report, err := VerifyDrive(context.Background(), cfg, cl)
			if err != nil {
				t.Fatal(err)
			}
			if report.Complete() != wantComplete || len(report.SkippedInternal) != wantSkipped {
				t.Fatalf("report=%+v, want complete=%v skipped=%d", report, wantComplete, wantSkipped)
			}
			if wantSkipped > 0 && report.RemoteFiles != 1 {
				t.Fatalf("artifacts affected normal count: %+v", report)
			}
			if mode == "temporary" || mode == "failed" || mode == "pending-backup" || mode == "pending-local-counterpart" {
				if len(report.Unverifiable) != 1 {
					t.Fatal("unfinished artifact was silently ignored")
				}
			}
			stateAfter, _ := os.ReadFile(cfg.StatePath())
			if string(stateAfter) != string(stateBefore) {
				t.Fatal("verification wrote synchronization state")
			}
		})
	}
}
