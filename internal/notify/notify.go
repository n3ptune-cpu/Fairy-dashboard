// Package notify sends an optional notification on control actions to FAIRY_NOTIFY_URL in fairy.env (an ntfy
// topic URL or a Discord webhook). Off unless set. Fire-and-forget; failures are logged at debug level and never block.
package notify

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// URL is FAIRY_NOTIFY_URL when it is an https:// URL, else "" (notifications off).
func URL() string {
	u := strings.TrimSpace(os.Getenv("FAIRY_NOTIFY_URL"))
	if strings.HasPrefix(u, "https://") {
		return u
	}
	return ""
}

var discordHosts = map[string]bool{"discord.com": true, "discordapp.com": true, "ptb.discord.com": true,
	"canary.discord.com": true}

// discordMarks are the characters Discord reads as Markdown or as its own marks (<@id>, <#id>, <t:...>, <url>).
const discordMarks = "\\*_~`|[]<>#"

// discordText is s as Discord shows it letter for letter: a backslash before every character it would read as
// Markdown. Title and message are plain text, and parts of them come from outside: a host's name from its agent,
// a guest's from Proxmox, a UPS's from the machine it hangs on. Without this a name such as
// "srv** [Session expired: sign in again](https://attacker.example/) **" was a masked link in the owner's
// channel. An address written out is still a link, to the address one reads. ntfy gets plain text as before.
func discordText(s string) string {
	if !strings.ContainsAny(s, discordMarks) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if r < 128 && strings.ContainsRune(discordMarks, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// BuildRequest returns the body, content type and extra headers for ntfy or Discord.
func BuildRequest(rawURL, title, message string) ([]byte, string, map[string]string) {
	u, _ := url.Parse(rawURL)
	host := ""
	if u != nil {
		host = u.Hostname()
	}
	if discordHosts[host] {
		c := "**" + discordText(title) + "**\n" + discordText(message)
		if r := []rune(c); len(r) > 1900 {
			c = string(r[:1900])
			if n := len(c) - len(strings.TrimRight(c, `\`)); n%2 == 1 {
				c = c[:len(c)-1] // the cut fell between a backslash and the character it stood before
			}
		}
		b, _ := json.Marshal(map[string]any{"content": c, "allowed_mentions": map[string]any{"parse": []string{}}})
		return b, "application/json", map[string]string{}
	}
	t := []byte(title)
	for i, c := range t {
		if c > 126 || c < 32 {
			t[i] = '?'
		}
	}
	return []byte(message), "text/plain; charset=utf-8", map[string]string{"Title": string(t), "Tags": "zap"}
}

var client = &http.Client{Timeout: 5 * time.Second}

func send(u, title, message string) {
	body, ct, hdr := BuildRequest(u, title, message)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", ct)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("notification failed")
		return
	}
	resp.Body.Close()
}

// Notify sends in the background.
func Notify(title, message string) {
	if u := URL(); u != "" {
		go send(u, title, message)
	}
}
