package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// socket_links and home_assistant_system in an agent's entry (build 17): what hangs on a socket of a plug the
// agent relays, and the row of the machine its Home Assistant runs on. The panel acts on them when a socket is
// switched, so a line that can never work is refused when the file is loaded, with the agent and the line.
func TestSocketLinksAndHomeAssistantSystem(t *testing.T) {
	entry := func(more string) string {
		return base + "agents:\n  - {id: mini-agent, url: \"http://192.0.2.7:9101\", token_env: T, " + more + "}\n"
	}
	c, err := Parse([]byte(entry(`socket_links: {desk-bar/ch3: bob-htpc, desk-bar/ch1: lab-node-pve-a, own-plug/main: lab-qemu-100}, home_assistant_system: lab-qemu-104`)))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Agents[0]
	if len(a.SocketLinks) != 3 || a.SocketLinks["desk-bar/ch3"] != "bob-htpc" || a.HomeAssistantSystem != "lab-qemu-104" {
		t.Fatalf("%+v", a)
	}
	// without the keys nothing is set, and a file from before build 17 loads as it did
	if c, err := Parse([]byte(entry(`power_links: {desk-bar: bob-htpc}`))); err != nil || c.Agents[0].SocketLinks != nil || c.Agents[0].HomeAssistantSystem != "" {
		t.Fatalf("an entry without the keys: %v %+v", err, c)
	}
	long := strings.Repeat("a", 41)
	for _, tc := range []struct{ more, want string }{
		{`socket_links: {desk-bar: bob-htpc}`, `agent mini-agent: socket_links: "desk-bar" is not "<device id>/<socket id>"`},
		{`socket_links: {"desk-bar/": bob-htpc}`, `socket_links: "desk-bar/" is not`},
		{`socket_links: {"/ch1": bob-htpc}`, `socket_links: "/ch1" is not`},
		{`socket_links: {"Desk-Bar/ch1": bob-htpc}`, `socket_links: "Desk-Bar/ch1" is not`},
		{`socket_links: {"desk-bar/CH1": bob-htpc}`, `socket_links: "desk-bar/CH1" is not`},
		{`socket_links: {"desk-bar/ch1/x": bob-htpc}`, `socket_links: "desk-bar/ch1/x" is not`},
		{`socket_links: {"mini-agent/desk-bar/ch1": bob-htpc}`, `is not "<device id>/<socket id>"`},
		{`socket_links: {"desk-bar/this-socket-id-is-long": bob-htpc}`, `is not "<device id>/<socket id>"`},
		{`socket_links: {"` + long + `/ch1": bob-htpc}`, `is not "<device id>/<socket id>"`},
		{`socket_links: {desk-bar/ch1: ""}`, `socket_links: desk-bar/ch1 is linked to nothing`},
		{`socket_links: {desk-bar/ch1: "Bob's PC"}`, `socket_links: desk-bar/ch1: "Bob's PC" is not an id a row can have`},
		{`socket_links: [desk-bar/ch1]`, `cannot unmarshal`}, // the loader's own words, with the line
		{`home_assistant_system: "Home Assistant"`, `agent mini-agent: home_assistant_system: "Home Assistant" is not an id a row can have`},
		{`home_assistant_system: [lab-qemu-104]`, `cannot unmarshal`},
	} {
		if _, err := Parse([]byte(entry(tc.more))); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", tc.more, err, tc.want)
		}
	}
	// more lines than an agent can have sockets
	var many []string
	for i := range MaxSocketLinks + 1 {
		many = append(many, fmt.Sprintf("p%d/ch1: bob-htpc", i))
	}
	if _, err := Parse([]byte(entry("socket_links: {" + strings.Join(many, ", ") + "}"))); err == nil || !strings.Contains(err.Error(), "an agent relays at most 128") {
		t.Errorf("too many lines: %v", err)
	}
	if _, err := Parse([]byte(entry("socket_links: {" + strings.Join(many[:MaxSocketLinks], ", ") + "}"))); err != nil {
		t.Errorf("as many lines as an agent can have sockets: %v", err)
	}
	// the names of the settings are in the example file, commented out
	example, err := os.ReadFile(filepath.Join("..", "..", "deploy", "config.example.yaml"))
	if err != nil || !strings.Contains(string(example), "# socket_links: {") || !strings.Contains(string(example), "# home_assistant_system: ") {
		t.Errorf("deploy/config.example.yaml must show socket_links and home_assistant_system: %v", err)
	}
}

func TestSocketLinkName(t *testing.T) {
	if dev, sock, ok := SocketLinkName("desk-bar/ch1"); !ok || dev != "desk-bar" || sock != "ch1" {
		t.Fatal(dev, sock, ok)
	}
	for _, bad := range []string{"", "/", "desk-bar", "desk-bar/", "/ch1", "a/b/c", "desk bar/ch1", "desk-bar/ch 1", "-bar/ch1", "bar/-ch1"} {
		if _, _, ok := SocketLinkName(bad); ok {
			t.Errorf("%q is taken as the name of a socket", bad)
		}
	}
}
