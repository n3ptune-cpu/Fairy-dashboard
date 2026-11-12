//go:build !windows

package settings

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// r2-p2-10 (audit 5, round 2): a named pipe under the sign-in picture's name made the first request for the
// picture wait for ever, and every later one behind it: the public route GET /branding/wallpaper hung for
// good. A link to /dev/zero was read up to the size limit and thrown away. Neither is opened now.
func TestASignInPictureThatIsNotAPlainFileIsNotRead(t *testing.T) {
	tinyPNG := func() []byte { return testPictures(t)["image/png"] }
	for name, mk := range map[string]func(p string) error{
		"a named pipe":        func(p string) error { return syscall.Mkfifo(p, 0o600) },
		"a link to /dev/zero": func(p string) error { return os.Symlink("/dev/zero", p) },
		"a link to a picture": func(p string) error {
			pic := filepath.Join(filepath.Dir(p), "real.png")
			if err := os.WriteFile(pic, tinyPNG(), 0o600); err != nil {
				return err
			}
			return os.Symlink(pic, p)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "backgrounds"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := mk(filepath.Join(dir, "backgrounds", signInFile)); err != nil {
				t.Fatal(err)
			}
			s := NewStore(dir)
			got := make(chan *SignInPicture, 1)
			go func() { got <- s.SignIn() }()
			select {
			case p := <-got:
				if p != nil {
					t.Fatalf("it is served: %s, %d bytes", p.Type, len(p.Data))
				}
			case <-time.After(10 * time.Second):
				t.Fatal("reading the sign-in picture does not come back")
			}
			// a real upload takes its place
			if p, err := s.SaveSignIn(tinyPNG()); err != nil || p == nil || NewStore(dir).SignIn() == nil {
				t.Fatalf("an upload afterwards: %v", err)
			}
		})
	}
}
