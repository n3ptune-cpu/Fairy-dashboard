package web

import (
	"strings"
	"testing"
)

// L12 (UI side): Re-pair of a host that is its own Proxmox node offers "the node was renamed"; the tick goes
// into the request body, and the stored node name is shown through esc().
func TestUIRepairOffersNodeRenamed(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`<input type="checkbox" id="td-renamed"> The node was renamed: accept another Proxmox node name than <b>${esc(h.proxmox.node)}</b>`,
		`const renamed = !!$("#td-renamed")?.checked;`,
		`JSON.stringify(renamed ? { confirm: v, node_renamed: true } : { confirm: v })`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}
