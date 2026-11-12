package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
)

// The host's page has no "Approval box" any more: it has the button "Approval" in its header, and a dialog
// behind it (audit 6, r4-p3-05 and r4-b1-02). Three refusals, two log lines and the two guides still sent the
// admin to the box. No text a person reads says "Approval box" now: not in the panel's code, not in the page,
// not in the guides. (Tests may say it: they tell what it was.)
func TestNothingSendsTheAdminToAnApprovalBox(t *testing.T) {
	root := filepath.Join("..", "..")
	var files []string
	for _, dir := range []string{"internal", "cmd", "deploy"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(p, "_test.go") {
				return err
			}
			switch filepath.Ext(p) {
			case ".go", ".js", ".html", ".css", ".yaml", ".md", ".conf":
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"README.md", "TESTING-LOCAL.md"} {
		files = append(files, filepath.Join(root, name))
	}
	if len(files) < 100 {
		t.Fatalf("only %d files were found: the search does not see the tree", len(files))
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(strings.ToLower(line), "approval box") || strings.Contains(line, "**Approval** box") {
				t.Errorf("%s:%d still names an Approval box: %s", strings.TrimPrefix(p, root+string(filepath.Separator)), i+1, strings.TrimSpace(line))
			}
		}
	}
	// what the three refusals say instead: where the button is
	if got := approval.BindingText("ALPHA", approval.BindNoRecord, nil); !strings.HasSuffix(got, "an admin accepts it first (press Approval on its page).") {
		t.Errorf("not on record: %q", got)
	}
	if got := bindingRefusalText("ALPHA", approval.BindVanished, nil); !strings.HasSuffix(got, " An admin resolves it: press Approval on the host's page.") {
		t.Errorf("check gone: %q", got)
	}
}
