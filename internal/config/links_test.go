package config

import (
	"fmt"
	"strings"
	"testing"
)

// LinkProblems is the one check of the link fields (ups_powers, power_links, power_exclude_from_total): for
// what "Edit host" stores, for what the panel reads back from state.json, and as notes about config.yaml.
func TestLinkProblems(t *testing.T) {
	many := func(n int) []string {
		var out []string
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf("d%d", i))
		}
		return out
	}
	mapOf := func(keys []string, v string) map[string]string {
		m := map[string]string{}
		for _, k := range keys {
			m[k] = v
		}
		return m
	}
	upsOf := func(keys []string, rows ...string) map[string][]string {
		m := map[string][]string{}
		for _, k := range keys {
			m[k] = rows
		}
		return m
	}
	long := strings.Repeat("a", 129)
	for name, c := range map[string]struct {
		l    Links
		want []string // "field kind", in order
	}{
		"nothing":            {Links{}, nil},
		"empty, not nil":     {Links{UPSPowers: map[string][]string{}, PowerLinks: map[string]string{}, PowerExcludeFromTotal: []string{}}, nil},
		"what a config has":  {Links{UPSPowers: map[string][]string{"rack": {"lab-b-agent", "lab-node-pve-a"}, "APC_1.5:b-2": {}}, PowerLinks: map[string]string{"pc-plug": "lab-qemu-100", "spare": ""}, PowerExcludeFromTotal: []string{"corner-plug"}}, nil},
		"sixteen of each":    {Links{UPSPowers: upsOf(many(MaxLinkUPS), "pc"), PowerLinks: mapOf(many(MaxLinkDevices), "pc"), PowerExcludeFromTotal: many(MaxLinkDevices)}, nil},
		"seventeen UPSes":    {Links{UPSPowers: upsOf(many(MaxLinkUPS+1), "pc")}, []string{"ups_powers count"}},
		"seventeen links":    {Links{PowerLinks: mapOf(many(MaxLinkDevices+1), "pc")}, []string{"power_links count"}},
		"seventeen left out": {Links{PowerExcludeFromTotal: many(MaxLinkDevices + 1)}, []string{"power_exclude_from_total count"}},
		"a UPS with 33 rows": {Links{UPSPowers: upsOf([]string{"rack"}, many(MaxLinkRows+1)...)}, []string{"ups_powers count"}},
		"a UPS with 32 rows": {Links{UPSPowers: upsOf([]string{"rack"}, many(MaxLinkRows)...)}, nil},
		"UPS names":          {Links{UPSPowers: upsOf([]string{"", "rack ups", "räck", strings.Repeat("u", 65), "a/b"})}, []string{"ups_powers ups", "ups_powers ups", "ups_powers ups", "ups_powers ups", "ups_powers ups"}},
		"a UPS name of 64":   {Links{UPSPowers: upsOf([]string{strings.Repeat("u", 64)})}, nil},
		"row ids of a UPS":   {Links{UPSPowers: upsOf([]string{"rack"}, "", "PC", "pc b", "-pc", long, "pc\n", "../pc")}, []string{"ups_powers row", "ups_powers row", "ups_powers row", "ups_powers row", "ups_powers row", "ups_powers row", "ups_powers row"}},
		"a row id of 128":    {Links{UPSPowers: upsOf([]string{"rack"}, long[:128])}, nil},
		"a row twice":        {Links{UPSPowers: upsOf([]string{"rack"}, "pc", "nas", "pc")}, []string{"ups_powers twice"}},
		"device ids":         {Links{PowerLinks: map[string]string{"Bad_ID": "pc", "": "pc", strings.Repeat("d", 64): "pc"}}, []string{"power_links device", "power_links device", "power_links device"}},
		"a link's row":       {Links{PowerLinks: map[string]string{"plug": "Lab B", "other": long}}, []string{"power_links row", "power_links row"}},
		"left out: ids":      {Links{PowerExcludeFromTotal: []string{"Bad_ID", "ok", "ok", ""}}, []string{"power_exclude_from_total device", "power_exclude_from_total device", "power_exclude_from_total twice"}},
		"one of each field":  {Links{UPSPowers: upsOf([]string{"a b"}), PowerLinks: map[string]string{"X": "pc"}, PowerExcludeFromTotal: []string{"Y"}}, []string{"ups_powers ups", "power_links device", "power_exclude_from_total device"}},
	} {
		var got []string
		for _, p := range LinkProblems(c.l) {
			got = append(got, p.Field+" "+p.Kind)
			if !strings.HasPrefix(p.Text, p.Field+": ") || strings.ContainsAny(p.Text, "\n\r") || len(p.Text) > 400 {
				t.Errorf("%s: the text of a problem is one line that begins with its field: %q", name, p.Text)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	// the same input, the same lines in the same order
	l := Links{UPSPowers: upsOf([]string{"b b", "a a", "c c"}), PowerLinks: map[string]string{"B": "x", "A": "x", "C": "x"}}
	first := fmt.Sprint(LinkProblems(l))
	for i := 0; i < 20; i++ {
		if fmt.Sprint(LinkProblems(l)) != first {
			t.Fatal("LinkProblems is not in a fixed order")
		}
	}
}

// config.yaml is never refused for its link fields: a file with every kind of problem in them loads as it
// did before the check existed (the check is for the panel's own overrides, and for notes).
func TestLinkProblemsNeverStopAConfigFromLoading(t *testing.T) {
	t.Setenv("TOK_R", "r")
	var devs []string
	for i := 0; i < 40; i++ {
		devs = append(devs, fmt.Sprintf("d%d: pc", i))
	}
	y := "public_url: https://p.example\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
		"agents:\n  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R,\n" +
		"     ups_powers: {\"rack ups\": [PC, \"\", pc, pc], \"" + strings.Repeat("u", 80) + "\": []},\n" +
		"     power_links: {Bad_ID: \"Lab B\", " + strings.Join(devs, ", ") + "},\n" +
		"     power_exclude_from_total: [Bad_ID, x, x]}\n"
	cfg, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("a config.yaml with odd link fields no longer loads: %v", err)
	}
	a := cfg.Agents[0]
	if len(a.PowerLinks) != 41 || len(a.UPSPowers) != 2 || len(a.PowerExcludeFromTotal) != 3 {
		t.Fatalf("the fields are not as written: %+v", a.Links())
	}
	if len(LinkProblems(a.Links())) < 8 {
		t.Fatalf("the check finds what is odd about them: %v", LinkProblems(a.Links()))
	}
	// Links hands out the entry's own fields; Clone shares nothing and keeps "empty" apart from "not there"
	c := a.Links().Clone()
	c.PowerLinks["d0"], c.UPSPowers["rack ups"][0], c.PowerExcludeFromTotal[0] = "other", "other", "other"
	if a.PowerLinks["d0"] != "pc" || a.UPSPowers["rack ups"][0] != "PC" || a.PowerExcludeFromTotal[0] != "Bad_ID" {
		t.Fatal("Clone shares with the entry")
	}
	e := Links{UPSPowers: map[string][]string{}, PowerLinks: map[string]string{}, PowerExcludeFromTotal: []string{}}.Clone()
	if e.UPSPowers == nil || e.PowerLinks == nil || e.PowerExcludeFromTotal == nil {
		t.Fatal("Clone made an empty field nil")
	}
	if n := (Links{}).Clone(); n.UPSPowers != nil || n.PowerLinks != nil || n.PowerExcludeFromTotal != nil {
		t.Fatal("Clone made a nil field empty")
	}
}
