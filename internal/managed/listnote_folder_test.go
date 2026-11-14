package managed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The page's line about an emptied list has no folder in it, however data_dir is written in config.yaml: with
// a slash at its end ("/var/lib/fairy/" is as usual as "/var/lib/fairy"), with two, with "/." or with a step
// up and down again. The line was made by taking "<data_dir>/" out of the log's line: with the folder written
// as "<dir>/" that looked for "<dir>//", which is in no path, and the page showed where the panel keeps its
// tokens (check of the build 15 candidate, L1).
func TestListNoteHasNoFolderHoweverDataDirIsWritten(t *testing.T) {
	for _, written := range []string{"", "/", "//", "/.", "/./", "/secrets/..", "/secrets/../"} {
		dir := t.TempDir()
		s := NewStore(dir)
		if err := s.Put(Host{ID: "box-a", Name: "BOX-A", URL: "http://box-a:9101", OS: "linux"}, Secret{Token: strings.Repeat("t", 32)}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.Path(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		want := s.ListNote() // the folder in its shortest form: what TestListNoteNamesTheFilesWithoutTheirFolder holds
		if strings.Contains(want, dir) || !strings.Contains(want, "are still in secrets (box-a)") || !strings.Contains(want, "Put managed-hosts.yaml back from a backup") {
			t.Fatalf("data_dir in its shortest form: %q", want)
		}
		as := NewStore(dir + written)
		if got := as.ListNote(); got != want {
			t.Errorf("data_dir written as <dir>%s: the page's line is\n  %q\nand not\n  %q", written, strings.ReplaceAll(got, dir, "<dir>"), want)
		}
		// the line for the log and for `fairy admin config check` keeps the folder: it is read on the panel's host
		if logged := as.emptyList(); !strings.Contains(logged, filepath.Join(dir, "secrets")) || !strings.Contains(logged, filepath.Join(dir, FileName)) {
			t.Errorf("data_dir written as <dir>%s: the log's line lost the folder: %q", written, logged)
		}
	}
}

// What is taken out of the line is what filepath.Join puts in front of a file's name, for every way to write
// a folder; a root keeps its one separator and gets no second.
func TestFolderPrefixIsWhatEveryPathOfTheStoreStartsWith(t *testing.T) {
	sep := string(filepath.Separator)
	for dir, want := range map[string]string{
		filepath.Join(sep, "var", "lib", "fairy"):                          filepath.Join(sep, "var", "lib", "fairy") + sep,
		filepath.Join(sep, "var", "lib", "fairy") + sep:                    filepath.Join(sep, "var", "lib", "fairy") + sep,
		filepath.Join(sep, "var", "lib", "fairy") + sep + sep:              filepath.Join(sep, "var", "lib", "fairy") + sep,
		filepath.Join(sep, "var", "lib", "fairy") + sep + ".":              filepath.Join(sep, "var", "lib", "fairy") + sep,
		sep + "var" + sep + "lib" + sep + "x" + sep + ".." + sep + "fairy": filepath.Join(sep, "var", "lib", "fairy") + sep,
		"data":       "data" + sep,
		"data" + sep: "data" + sep,
		sep:          sep,
	} {
		got := folderPrefix(dir)
		if got != want {
			t.Errorf("folderPrefix(%q) = %q, want %q", dir, got, want)
		}
		if path := filepath.Join(dir, "secrets"); strings.ReplaceAll(path, got, "") != "secrets" {
			t.Errorf("%q: the path %q without %q is %q", dir, path, got, strings.ReplaceAll(path, got, ""))
		}
	}
}
