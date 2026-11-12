package web

import (
	"testing"
)

// r2-b1-12 (bug run of audit 5): the Sites table of the Users page said "Hosts 11" for a site with 4 hosts. The
// number counted everything that is in the site: its hosts, their guests, power sources and UPSes. The column
// says "Hosts", and the table of hosts is right below it, so it counts hosts: Proxmox nodes and machines with
// an agent.
func TestASitesHostCountCountsHosts(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	r := e.do("GET", "/api/admin/users", nil, "Cookie", c)
	if r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	// everything the page lists (hosts, guests, power sources, UPSes) goes into one site
	if err := e.s.Access.CreateSite("Home"); err != nil {
		t.Fatal(err)
	}
	for _, x := range r.body["targets"].([]any) {
		if _, err := e.s.Access.Assign(x.(map[string]any)["target"].(string), "Home"); err != nil {
			t.Fatal(err)
		}
	}
	r = e.do("GET", "/api/admin/users", nil, "Cookie", c)
	hosts, others := map[string]int{}, map[string]int{}
	for _, x := range r.body["targets"].([]any) {
		tg := x.(map[string]any)
		site, _ := tg["site"].(string)
		if tg["kind"] == "node" || tg["kind"] == "agent" {
			hosts[site]++
		} else {
			others[site]++
		}
	}
	sawOthers := false
	for _, x := range r.body["sites"].([]any) {
		st := x.(map[string]any)
		name := st["name"].(string)
		if got := int(st["hosts"].(float64)); got != hosts[name] {
			t.Errorf("site %s: hosts %d, the page lists %d hosts in it (and %d other things)", name, got, hosts[name], others[name])
		}
		sawOthers = sawOthers || others[name] > 0
	}
	if len(r.body["sites"].([]any)) == 0 || !sawOthers {
		t.Fatalf("the fixture has no site with guests or power sources in it: %v / %v", hosts, others)
	}
}
