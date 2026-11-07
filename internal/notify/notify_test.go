package notify

import (
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
