package admin

import (
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"
)

func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return fmt.Sprintf("uid %d", os.Getuid())
}

func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	s := time.Since(*t).Seconds()
	switch {
	case s < 120:
		return fmt.Sprintf("%.0fs ago", s)
	case s < 7200:
		return fmt.Sprintf("%.0fm ago", s/60)
	case s < 172800:
		return fmt.Sprintf("%.1fh ago", s/3600)
	}
	return fmt.Sprintf("%.0fd ago", s/86400)
}

func agoISO(s string) string {
	if s == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "?"
	}
	return ago(&t)
}

func table(rows [][]string, head []string) string {
	w := make([]int, len(head))
	for _, r := range rows {
		cells(r...) // one line per row whatever a field holds; widths of what is printed
	}
	for _, r := range append([][]string{head}, rows...) {
		for i, c := range r {
			w[i] = max(w[i], len([]rune(c)))
		}
	}
	line := func(r []string) string {
		parts := make([]string, len(r))
		for i, c := range r {
			parts[i] = c + strings.Repeat(" ", w[i]-len([]rune(c)))
		}
		return strings.TrimRight(strings.Join(parts, "  "), " ")
	}
	dash := make([]string, len(head))
	for i := range head {
		dash[i] = strings.Repeat("-", w[i])
	}
	out := []string{line(head), line(dash)}
	for _, r := range rows {
		out = append(out, line(r))
	}
	return strings.Join(out, "\n")
}

func cut(v any, n int) string {
	t := ""
	if v != nil {
		t = cell(fmt.Sprint(v))
	}
	if t == "" {
		t = "-"
	}
	r := []rune(t)
	if len(r) <= n {
		return t
	}
	return string(r[:n-1]) + "…"
}

func pad(s string, n int) string {
	if l := len([]rune(s)); l < n {
		return s + strings.Repeat(" ", n-l)
	}
	return s
}
