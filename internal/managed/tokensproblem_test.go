package managed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What is wrong with a host's tokens file, for a page in a browser (check of the build 15 candidate, K2): the
// file's name without its folder and one of a few fixed sentences. The error of Secret, which the log and
// `fairy admin config check` show, has the full path and, from a file that is not JSON, the character the
// decoder stopped at; none of that is in the sentence, whatever is in the file.
func TestTokensProblemIsSaidInFixedWordsWithoutTheFolderOrTheFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Put(Host{ID: "box-a", Name: "BOX-A", URL: "http://box-a:9101", OS: "linux"}, Secret{Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "secrets", "agent-box-a.json")
	if got := s.TokensProblem("box-a"); got != "" {
		t.Fatalf("a good file: %q", got)
	}
	for name, c := range map[string]struct {
		make func() error
		want string
	}{
		"not JSON":      {func() error { return os.WriteFile(file, []byte(`{"token": "ZZ-in-the-file-ZZ" ]`), 0o600) }, "its tokens file agent-box-a.json is not valid JSON"},
		"cut off":       {func() error { return os.WriteFile(file, []byte(`{"token": "ZZ-in-the-fi`), 0o600) }, "its tokens file agent-box-a.json is not valid JSON"},
		"empty":         {func() error { return os.WriteFile(file, nil, 0o600) }, "its tokens file agent-box-a.json is not valid JSON"},
		"another shape": {func() error { return os.WriteFile(file, []byte(`{"token": ["ZZ-in-the-file-ZZ"]}`), 0o600) }, "its tokens file agent-box-a.json is not valid JSON"},
		"a short token": {func() error { return os.WriteFile(file, []byte(`{"token": "ZZ-short-ZZ"}`), 0o600) }, "its tokens file agent-box-a.json does not hold usable tokens"},
		"the same twice": {func() error {
			t := strings.Repeat("Z", 32)
			return os.WriteFile(file, []byte(`{"token": "`+t+`", "control_token": "`+t+`"}`), 0o600)
		}, "its tokens file agent-box-a.json does not hold usable tokens"},
		"missing":         {func() error { return nil }, "its tokens file agent-box-a.json is missing"},
		"a folder":        {func() error { return os.Mkdir(file, 0o700) }, "its tokens file agent-box-a.json is not a plain file"},
		"a symbolic link": {func() error { return os.Symlink(filepath.Join(dir, "elsewhere"), file) }, "its tokens file agent-box-a.json is not a plain file"},
	} {
		_ = os.RemoveAll(file)
		if err := c.make(); err != nil {
			t.Fatal(name, err)
		}
		_, err := s.Secret("box-a")
		got := s.TokensProblem("box-a")
		if err == nil || got != c.want {
			t.Errorf("%s: %q, want %q (Secret: %v)", name, got, c.want, err)
		}
		if strings.Contains(got, dir) || strings.Contains(got, "ZZ") || strings.Contains(got, "invalid character") || strings.Contains(got, "/") {
			t.Errorf("%s: the sentence has the folder or something of the file in it: %q", name, got)
		}
	}
	// the error itself, for the log, keeps saying where and what
	_ = os.RemoveAll(file)
	if err := os.WriteFile(file, []byte(`{"token": "ZZ" ]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Secret("box-a"); err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("the error for the log: %v", err)
	}
	if err := os.WriteFile(file, []byte(`{"token": "short"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Secret("box-a"); err == nil || err.Error() != file+": tokens too short or equal" {
		t.Errorf("the error for the log, short tokens: %v", err)
	}
	if got := s.TokensProblem("No Such Id"); got != "its id is not one a host can have" {
		t.Errorf("an id that is none: %q", got)
	}
}

// The line about a list that holds no host although tokens of added hosts are there, for the page: the words
// of the log's line with the names of the files, without the folder; and no line while the list holds a host.
func TestListNoteNamesTheFilesWithoutTheirFolder(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if got := s.ListNote(); got != "" {
		t.Fatalf("nothing there: %q", got)
	}
	if err := s.Put(Host{ID: "box-a", Name: "BOX-A", URL: "http://box-a:9101", OS: "linux"}, Secret{Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	if got := s.ListNote(); got != "" {
		t.Fatalf("a list with a host in it: %q", got)
	}
	if err := os.WriteFile(s.Path(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, logged := s.ListNote(), s.emptyList()
	if !strings.Contains(logged, dir) || strings.Contains(got, dir) || got != strings.ReplaceAll(logged, dir+string(filepath.Separator), "") ||
		!strings.Contains(got, "are still in secrets (box-a)") || !strings.Contains(got, "Put managed-hosts.yaml back from a backup") {
		t.Errorf("the page's line: %q\nthe log's line:  %q", got, logged)
	}
}
