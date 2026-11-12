package web

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

func heapMiB() float64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / (1 << 20)
}

// unread opens a connection to the test panel that takes almost nothing (a 2 kB receive buffer), sends req and
// does not read the answer.
func (e *env) unread(req string) net.Conn {
	e.t.Helper()
	u, _ := url.Parse(e.srv.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { conn.Close() })
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(2048)
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		e.t.Fatal(err)
	}
	return conn
}

func (e *env) uploadBackground(cookie string, data []byte) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/settings/background", bytes.NewReader(data))
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-Requested-With", "fairy")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 201 {
		e.t.Fatal("upload:", r.StatusCode)
	}
}

// T-09: a signed-in viewer's own background image (8 MiB are allowed) was read into memory for every GET and
// held for as long as the client did not take the answer: 8 MiB per connection, about 60 connections to the
// unit's 512M limit. Now the file is streamed, one user has at most maxBackgroundsPerUser downloads in
// flight (the next one is answered 429), and nobody else is affected.
func TestBackgroundDownloadIsStreamedAndLimitedPerUser(t *testing.T) {
	e := setup(t)
	alice, bob := viewer, viewer
	bob.Sub, bob.Email = "u7", nil
	ca, cb := e.cookie(alice, 0), e.cookie(bob, 0)
	for _, sub := range []string{alice.Sub, bob.Sub} {
		if _, _, err := e.s.Access.SetRole(sub, "viewer", []string{}, "test", true); err != nil {
			t.Fatal(err)
		}
	}
	png, small := pngOfSize(t, 8<<20), pngOfSize(t, 4096)
	e.uploadBackground(ca, png)
	e.uploadBackground(cb, small)

	base := heapMiB()
	const held = 20
	get := "GET /api/settings/background HTTP/1.1\r\nHost: x\r\nUser-Agent: " + testUA + "\r\nCookie: " + ca + "\r\n\r\n"
	for range held {
		e.unread(get)
	}
	time.Sleep(500 * time.Millisecond) // the handlers are in their writes now
	if grew := heapMiB() - base; grew > 16 {
		t.Fatalf("%d downloads that are not read hold %.0f MiB", held, grew)
	}
	// the user is at the limit: one more is refused, at once, and says when to come back
	r := e.do("GET", "/api/settings/background", nil, "Cookie", ca)
	if r.code != 429 || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("download number %d of one user: %d", held+1, r.code)
	}
	// another user's download works, whole and with the headers it had
	r = e.do("GET", "/api/settings/background", nil, "Cookie", cb)
	if r.code != 200 || !bytes.Equal(r.raw, small) || r.hdr.Get("Content-Type") != "image/png" ||
		r.hdr.Get("Content-Security-Policy") != "default-src 'none'" || r.hdr.Get("Content-Disposition") != "inline" ||
		r.hdr.Get("Cache-Control") != "private, max-age=86400" {
		t.Fatalf("another user's download: %d, %d bytes, %v", r.code, len(r.raw), r.hdr)
	}
	// the user's own page still works
	if r := e.do("GET", "/api/me", nil, "Cookie", ca); r.code != 200 {
		t.Fatal("/api/me:", r.code)
	}
}

// T-09: the slots come back when the downloads end, and a whole 8 MiB download arrives intact.
func TestBackgroundDownloadSlotsAreReleased(t *testing.T) {
	e := setup(t)
	ck := e.cookie(viewer, 0)
	if _, _, err := e.s.Access.SetRole(viewer.Sub, "viewer", []string{}, "test", true); err != nil {
		t.Fatal(err)
	}
	png := pngOfSize(t, 8<<20)
	e.uploadBackground(ck, png)
	get := "GET /api/settings/background HTTP/1.1\r\nHost: x\r\nUser-Agent: " + testUA + "\r\nCookie: " + ck + "\r\n\r\n"
	var conns []net.Conn
	for range maxBackgroundsPerUser {
		conns = append(conns, e.unread(get))
	}
	time.Sleep(300 * time.Millisecond)
	if r := e.do("GET", "/api/settings/background", nil, "Cookie", ck); r.code != 429 {
		t.Fatalf("with %d in flight: %d", maxBackgroundsPerUser, r.code)
	}
	for _, c := range conns {
		c.Close()
	}
	var r resp
	for i := 0; i < 50; i++ {
		if r = e.do("GET", "/api/settings/background", nil, "Cookie", ck); r.code == 200 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if r.code != 200 || !bytes.Equal(r.raw, png) {
		t.Fatalf("after the held downloads were closed: %d, %d bytes", r.code, len(r.raw))
	}
	// a part of it (Range), as a browser may ask
	r = e.do("GET", "/api/settings/background", nil, "Cookie", ck, "Range", "bytes=0-7")
	if r.code != 206 || string(r.raw) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("range request: %d %q", r.code, r.raw)
	}
}

// T-33: the server had no write timeout, so an answer nobody reads held its connection, its goroutine and
// its buffers for as long as the client liked (pipelined GET /static/app.js, no sign-in needed). Now an
// answer must be taken within answerTimeout of its first byte, or the connection is closed.
func TestAnAnswerNobodyReadsEndsTheConnection(t *testing.T) {
	e := setup(t)
	e.s.answerTimeout = 400 * time.Millisecond
	one := e.do("GET", "/static/app.js", nil)
	if one.code != 200 || len(one.raw) < 50_000 {
		t.Fatal("app.js:", one.code, len(one.raw))
	}
	const n = 200 // far more than the socket buffers of both ends take
	conn := e.unread(strings.Repeat("GET /static/app.js HTTP/1.1\r\nHost: x\r\n\r\n", n))
	time.Sleep(4 * e.s.answerTimeout)
	// now read: a connection the panel gave up delivers what was in flight and then ends
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.Copy(io.Discard, conn)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("the connection is still open after %v without reading (%d bytes arrived)", 4*e.s.answerTimeout, got)
	}
	if got >= int64(n*len(one.raw)) {
		t.Fatalf("all %d answers were delivered: the panel waited for a client that did not read", n)
	}
}

// T-33: the write deadline is for an answer that is not taken. It does not cut a handler that needs longer
// than answerTimeout before it answers, and it does not end the live stream, which writes for hours.
func TestWriteDeadlineLeavesSlowHandlersAndTheStreamAlone(t *testing.T) {
	e := setup(t)
	e.s.answerTimeout = 300 * time.Millisecond
	e.s.mux.HandleFunc("GET /zz-slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * e.s.answerTimeout)
		_, _ = w.Write([]byte("late but whole"))
	})
	e.s.mux.HandleFunc("GET /zz-empty", func(http.ResponseWriter, *http.Request) {}) // answers 200 without writing
	// one connection: an answer, a pause longer than the deadline, then a slow and an empty answer
	u, _ := url.Parse(e.srv.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	ask := func(path string) (int, string) {
		t.Helper()
		if _, err := conn.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		r, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		return r.StatusCode, string(b)
	}
	if code, _ := ask("/healthz"); code != 200 {
		t.Fatal("healthz", code)
	}
	time.Sleep(2 * e.s.answerTimeout)
	if code, body := ask("/zz-slow"); code != 200 || body != "late but whole" {
		t.Fatalf("a handler slower than the write deadline: %d %q", code, body)
	}
	time.Sleep(2 * e.s.answerTimeout)
	if code, _ := ask("/zz-empty"); code != 200 {
		t.Fatal("an answer without a body on a connection that was quiet for longer than the deadline:", code)
	}
	if code, _ := ask("/healthz"); code != 200 {
		t.Fatal("healthz again", code)
	}

	// the live stream: events keep arriving long after answerTimeout
	ck := e.cookie(controller(), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/stream", nil)
	req.Header.Set("Cookie", ck)
	r, err := http.DefaultClient.Do(req)
	if err != nil || r.StatusCode != 200 {
		t.Fatal("stream:", err, r)
	}
	defer r.Body.Close()
	sc := bufio.NewReader(r.Body)
	waitFor := func(event string) {
		t.Helper()
		for {
			line, err := sc.ReadString('\n')
			if err != nil {
				t.Fatalf("the stream ended while waiting for %q: %v", event, err)
			}
			if strings.HasPrefix(line, "event: "+event) {
				return
			}
		}
	}
	waitFor("systems")
	for i := 0; i < 3; i++ { // three more events, each later than the deadline of the one before
		time.Sleep(2 * e.s.answerTimeout)
		e.b.Mu.Lock()
		m := e.b.Metrics["laptop"]
		e.b.Metrics["laptop"] = func() (jsonx.M, error) { // the laptop reports something new
			v, err := m()
			if v != nil {
				v["uptime_s"] = 7200.0 + float64(i)
			}
			return v, err
		}
		e.b.Mu.Unlock()
		e.s.Poller.PollOnce(context.Background())
		waitFor("systems_delta")
	}
}
