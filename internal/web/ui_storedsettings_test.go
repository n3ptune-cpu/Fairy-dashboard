package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// r1-p3-06 (audit 5): when GET /api/settings failed, the page took whatever was under "fairy-settings" in the
// browser's storage as its settings, unchecked. "{}" or a text there left the page without its hosts (an error
// in every render), a number as text went into the page's style, and any address stored as the background
// was loaded. The stored copy is now used only if it is what the page itself writes; otherwise the defaults.
func TestUIStoredSettingsAreCheckedBeforeUse(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`function storedSettings(raw) {`,
		`state.settings = storedSettings(raw); }`,
		`if (u.protocol !== "https:" || !u.host) return null;`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if strings.Contains(js, `state.settings = JSON.parse(localStorage.getItem(LS_KEY))`) {
		t.Error("app.js still uses the stored settings as they are")
	}

	t.Run("in node, with the auditor's stored values", func(t *testing.T) {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skip("node is not installed: the page's own function is not run here")
		}
		fn := regexp.MustCompile(`(?ms)^function storedSettings\(raw\) \{.*?^\}$`).FindString(js)
		if fn == "" {
			t.Fatal("app.js has no storedSettings any more")
		}
		script := `"use strict";
` + fn + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const good = { theme: "black", background: { kind: "url", url: "https://pictures.example/wall.jpg", version: 3 }, dim: 60, blur: 14, card_opacity: 72 };
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
// what the page writes comes back as it is (with and without pop-up settings)
if (!same(storedSettings(JSON.stringify(good)), good)) fail("a good copy: " + JSON.stringify(storedSettings(JSON.stringify(good))));
const pop = { ...good, background: { kind: "upload", url: null, version: 9 }, popups: { enabled: false, seconds: 8, level: "warning" } };
if (!same(storedSettings(JSON.stringify(pop)), pop)) fail("a good copy with pop-ups: " + JSON.stringify(storedSettings(JSON.stringify(pop))));
const none = { theme: "black", background: { kind: "none", url: null, version: 0 }, dim: 0, blur: 40, card_opacity: 20 };
if (!same(storedSettings(JSON.stringify(none)), none)) fail("the edges of the ranges");
// the four values of the audit, and their relatives: not used
const bad = {
  "nothing stored": null, "not JSON": "{", "empty object": "{}", "a string": '"x"', "null": "null", "a list": "[]", "a number": "7",
  "numbers as CSS": JSON.stringify({ background: { kind: "none" }, dim: "1;background:red", blur: "0px;position:fixed", card_opacity: 72 }),
  "no background": JSON.stringify({ ...good, background: undefined }), "background as text": JSON.stringify({ ...good, background: "url" }),
  "dim out of range": JSON.stringify({ ...good, dim: 91 }), "blur out of range": JSON.stringify({ ...good, blur: -1 }),
  "opacity out of range": JSON.stringify({ ...good, card_opacity: 19 }), "a fraction": JSON.stringify({ ...good, dim: 60.5 }),
  "an unknown kind": JSON.stringify({ ...good, background: { kind: "file", url: "https://a.example/x" } }),
  "http": JSON.stringify({ ...good, background: { kind: "url", url: "http://tracker.example/t.png" } }),
  "javascript": JSON.stringify({ ...good, background: { kind: "url", url: "javascript:alert(1)" } }),
  "data": JSON.stringify({ ...good, background: { kind: "url", url: "data:image/png;base64,AAAA" } }),
  "no host": JSON.stringify({ ...good, background: { kind: "url", url: "https://" } }),
  "relative": JSON.stringify({ ...good, background: { kind: "url", url: "/api/admin/users" } }),
  "url is not text": JSON.stringify({ ...good, background: { kind: "url", url: { toString: 1 } } }),
  "url with a line end": JSON.stringify({ ...good, background: { kind: "url", url: "https://a.example/x\n)" } }),
  "url too long": JSON.stringify({ ...good, background: { kind: "url", url: "https://a.example/" + "x".repeat(2100) } }),
};
for (const [name, raw] of Object.entries(bad)) if (storedSettings(raw) !== null) fail(name + " was used: " + JSON.stringify(storedSettings(raw)));
// what is taken over is only what the page knows: nothing else of the stored object, and no address without kind=url
const extra = storedSettings(JSON.stringify({ ...good, background: { kind: "none", url: "https://tracker.example/t.png", version: -4, more: 1 }, theme: "x", other: 1, popups: "x" }));
if (!same(extra, { theme: "black", background: { kind: "none", url: null, version: 0 }, dim: 60, blur: 14, card_opacity: 72 })) fail("extras: " + JSON.stringify(extra));
if (!process.exitCode) console.log("ok " + Object.keys(bad).length + " refused");
`
		file := filepath.Join(t.TempDir(), "stored.js")
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, file).CombinedOutput()
		if err != nil || !strings.HasPrefix(string(out), "ok 23 refused") {
			t.Fatalf("node: %v\n%s", err, out)
		}
	})
}
