package config

import "testing"

func TestDriveMappingValidation(t *testing.T) {
	for _, tc := range []struct{ name, remote, local string }{
		{"absolute", "A", "/tmp/a"}, {"parent", "A", "a/../b"}, {"dot", "A", "a/./b"}, {"empty", "A", ""}, {"backslash", "A", `a\b`}, {"remote traversal", "A/../B", "a"}, {"remote absolute", "/A", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.DriveSubfolders = []DriveMapping{{tc.remote, tc.local}}
			if c.ValidateDriveMappings() == nil {
				t.Fatal("accepted invalid path")
			}
		})
	}
	for _, pair := range [][2]DriveMapping{
		{{"A", "a"}, {"B", "a"}}, {{"A", "a"}, {"B", "a/b"}}, {{"A", "a/b"}, {"B", "a"}}, {{"A", "a"}, {"A", "b"}}, {{"A", "a"}, {"A/B", "b"}}, {{"A/B", "a"}, {"A", "b"}}, {{"A", "a"}, {"B", "A"}},
	} {
		c := Defaults()
		c.DriveSubfolders = pair[:]
		if c.ValidateDriveMappings() == nil {
			t.Fatalf("accepted overlap: %v", pair)
		}
	}
	c := Defaults()
	c.DriveSubfolder = "A"
	c.DriveSubfolders = []DriveMapping{{"B", "b"}}
	if c.ValidateDriveMappings() == nil {
		t.Fatal("accepted both options")
	}
	c = Defaults()
	c.DriveSubfolders = []DriveMapping{}
	if c.ValidateDriveMappings() == nil {
		t.Fatal("accepted empty mappings")
	}
}
