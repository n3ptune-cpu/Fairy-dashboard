package web

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// r1-b1-06 (audit 5, bug run): the result of an action that ran after "Confirm it's you" was cut at 300 bytes.
// An Add host that failed because the panel could not reach GitHub has a longer sentence, and the page showed
// it ending in the middle of a word ("Add host shows no comman"). Such a sentence is whole now; one that is
// longer than the limit ends at a space, never inside a character, and says that it goes on.
func TestAKeptActionsResultIsNotCutInTheMiddleOfAWord(t *testing.T) {
	// the sentence of the auditor's run, as internal/enrollment/script.go makes it
	seen := `cannot check the Fairy-agent install script (release build-16 of n3ptune-cpu/Fairy-agent): Get "https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-16/SHA256SUMS": dial tcp: lookup github.com on 8.8.4.4:53: dial udp 8.8.4.4:53: connect: network is unreachable; Add host shows no command until it can`
	if len(seen) <= 300 {
		t.Fatal("the test's sentence is not one the old limit cut")
	}
	if got := cutMessage(seen); got != seen {
		t.Fatalf("the sentence is cut: %q", got)
	}
	for _, m := range []string{"", "done", strings.Repeat("x", maxResultMessage)} {
		if got := cutMessage(m); got != m {
			t.Errorf("a sentence inside the limit was changed: %d bytes -> %d", len(m), len(got))
		}
	}
	long := strings.Repeat("ein längerer Satz über Geräte ", 60) // 1860 bytes, with two-byte letters
	got := cutMessage(long)
	if len(got) > maxResultMessage+len("…") || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) || !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
		t.Fatalf("a long sentence: %d bytes, %q", len(got), got[len(got)-40:])
	}
	if cut := strings.TrimSuffix(got, "…"); long[len(cut)] != ' ' {
		t.Errorf("it does not end at a space: %q", cut[len(cut)-20:])
	}
	// no space near the limit, and the limit inside a character: cut before that character
	word := strings.Repeat("ä", maxResultMessage) // 2000 bytes; byte 1000 is the start of a letter, 999 is inside one
	if got := cutMessage("x" + word); !utf8.ValidString(got) || len(got) > maxResultMessage+len("…") || !strings.HasSuffix(got, "ä…") {
		t.Errorf("cut inside a character: %q", got[len(got)-12:])
	}
}
