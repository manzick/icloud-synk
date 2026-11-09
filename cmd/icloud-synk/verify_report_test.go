package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kolesnikav/icloud-synk/internal/synk"
)

func TestVerifyReportExitStatusContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		report  synk.VerifyReport
		success bool
	}{
		{"normal-match", synk.VerifyReport{LocalFiles: 1, RemoteFiles: 1, Identical: 1}, true},
		{"owned-backups-only", synk.VerifyReport{LocalFiles: 1, RemoteFiles: 1, Identical: 1, SkippedInternal: []string{"owned-backup"}}, true},
		{"historical-backups-only", synk.VerifyReport{LocalFiles: 1, RemoteFiles: 1, Identical: 1, SkippedInternal: []string{"backup (backup filename; provenance unavailable, ownership not established)"}}, true},
		{"ordinary-extra-file", synk.VerifyReport{RemoteFiles: 1, Differences: []string{"File missing locally: user.txt"}}, false},
		{"missing-remote", synk.VerifyReport{LocalFiles: 1, Differences: []string{"File missing in iCloud: user.txt"}}, false},
		{"corrupt", synk.VerifyReport{LocalFiles: 1, RemoteFiles: 1, Differences: []string{"Content or size differs: user.txt"}}, false},
		{"unfinished", synk.VerifyReport{SkippedInternal: []string{"pending"}, Unverifiable: []string{"Unfinished replacement artifact retained: pending"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := writeVerifyReport(&out, tc.report)
			// main maps nil to exit 0 and errors to exit 1, without requiring
			// authentication or live services to exercise this command contract.
			if (err == nil) != tc.success {
				t.Fatalf("success=%v, error=%v", tc.success, err)
			}
			if !strings.Contains(out.String(), "excluding skipped internal artifacts") {
				t.Fatalf("remote count scope missing: %s", out.String())
			}
			if !strings.Contains(out.String(), "Local files:") || !strings.Contains(out.String(), "RESULT:") || !strings.Contains(out.String(), "Skipped internal artifacts:") {
				t.Fatalf("incomplete English output: %s", out.String())
			}
		})
	}
}
