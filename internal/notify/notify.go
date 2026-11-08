// Package notify: optional notification on control actions. FAIRY_NOTIFY_URL in fairy.env (an ntfy topic URL
// or a Discord webhook). Off unless set. Fire-and-forget; failures are logged at debug level and never block.
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

func URL() string {
	u := strings.TrimSpace(os.Getenv("FAIRY_NOTIFY_URL"))
	if strings.HasPrefix(u, "https://") {
		return u
	}
	return ""
}

var discordHosts = map[string]bool{"discord.com": true, "discordapp.com": true, "ptb.discord.com": true,
	"canary.discord.com": true}

// BuildRequest returns the body, content type and extra headers for ntfy or Discord.
func BuildRequest(rawURL, title, message string) ([]byte, string, map[string]string) {
	u, _ := url.Parse(rawURL)
	host := ""
	if u != nil {
		host = u.Hostname()
	}
	if discordHosts[host] {
		c := "**" + title + "**\n" + message
		if r := []rune(c); len(r) > 1900 {
			c = string(r[:1900])
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
