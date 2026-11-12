package notify

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildRequest(t *testing.T) {
	b, ct, h := BuildRequest("https://discord.com/api/webhooks/1/x", "Fairy: power.shutdown", "@everyone hi")
	if ct != "application/json" || !strings.Contains(string(b), `"allowed_mentions":{"parse":[]}`) || len(h) != 0 {
		t.Fatal(string(b))
	}
	b, ct, h = BuildRequest("https://ntfy.sh/topic", "Fairy ⚡ test", "msg")
	if !strings.HasPrefix(ct, "text/plain") || string(b) != "msg" || h["Title"] != "Fairy ??? test" || h["Tags"] != "zap" {
		t.Fatal(h)
	}
	t.Setenv("FAIRY_NOTIFY_URL", "http://insecure.example")
	if URL() != "" {
		t.Fatal("only https URLs")
	}
}

// r2-p3-04: a host's name comes from its agent and went to Discord as Markdown: with the auditor's name the
// message was a masked link ("Session expired: sign in again") in the owner's channel. Discord now gets every
// mark with a backslash before it; ntfy gets the text as it is, as before.
func TestDiscordGetsNamesAsPlainText(t *testing.T) {
	content := func(title, message string) string {
		t.Helper()
		b, _, _ := BuildRequest("https://discord.com/api/webhooks/1/x", title, message)
		var m struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m.Content
	}
	host := "srv** [Session expired: sign in again](https://attacker.invalid/login) **"
	got := content("Fairy: "+host+": offline", "offline (critical) at 2026-11-12T00:00:00Z")
	want := "**Fairy: srv\\*\\* \\[Session expired: sign in again\\](https://attacker.invalid/login) \\*\\*: offline**\noffline (critical) at 2026-11-12T00:00:00Z"
	if got != want {
		t.Fatalf("Discord content:\n got %q\nwant %q", got, want)
	}
	// no mark is left that Discord would read: every one of them has a backslash before it
	plain := strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(got, "\n", 2)[0], "**"), "**")
	for i, r := range plain {
		if strings.ContainsRune(discordMarks, r) && r != '\\' && (i == 0 || plain[i-1] != '\\') {
			t.Errorf("%q at %d of %q is still Markdown", r, i, plain)
		}
	}
	for in, want := range map[string]string{
		"my_host_1":          `my\_host\_1`,
		"a`b` ~~c~~ ||d||":   "a\\`b\\` \\~\\~c\\~\\~ \\|\\|d\\|\\|",
		"<@123> <#5> > # x":  `\<@123\> \<\#5\> \> \# x`,
		`back\slash`:         `back\\slash`,
		"NAS (Büro) 12:30 ⚡": "NAS (Büro) 12:30 ⚡",
	} {
		if got := discordText(in); got != want {
			t.Errorf("discordText(%q) = %q, want %q", in, got, want)
		}
	}
	// the cut at 1900 characters does not leave half an escape at the end
	// ("**t**" and the line break are 6 of them)
	if long := content("t", strings.Repeat("a", 1893)+"*****"); !strings.HasSuffix(long, "aa") || len([]rune(long)) != 1899 {
		t.Errorf("cut after a backslash: ends with %q, %d characters", long[len(long)-6:], len([]rune(long)))
	}
	if long := content("t", strings.Repeat("a", 1892)+"*****"); !strings.HasSuffix(long, `aa\*`) || len([]rune(long)) != 1900 {
		t.Errorf("cut after a whole escape: ends with %q, %d characters", long[len(long)-6:], len([]rune(long)))
	}
	// ntfy: plain text, unchanged
	b, _, h := BuildRequest("https://ntfy.sh/topic", "Fairy: "+host+": offline", "a *b* [c](d)")
	if string(b) != "a *b* [c](d)" || h["Title"] != "Fairy: "+host+": offline" {
		t.Errorf("ntfy changed: %q %q", b, h["Title"])
	}
}
