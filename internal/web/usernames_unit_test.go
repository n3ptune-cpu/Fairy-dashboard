package web

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Tests of the web layer's own ways to name a person (people.go), asked directly. What the API answers is in
// usernames_test.go.

// A run as the Updates page gets it holds no address, whichever of its texts an older version (or a machine's
// answer) put one in. Every text field of a run is filled with an address here, by its type, so a field that
// is added later is in the check without anybody thinking of it: it either goes through runShown or is put on
// the list of the fields that never name a person, with the reason.
func TestARunIsHandedOutWithoutAnAddressInAnyText(t *testing.T) {
	// ids, states and versions the panel sets itself, and the names of machines
	never := map[string]string{
		"Run.ID": "an id", "Run.State": "a state", "Run.BySub": "the account id", "Run.ListHash": "a hash", "Run.Latest": "a version",
		"Run.Approval": "a state", "Run.ApprovalEnd": "a state",
		"Host.ID": "an id", "Host.Name": "the machine's name", "Host.Mode": "a state", "Host.State": "a state", "Host.StartStatus": "a state", "Host.Asking": "a state",
		"Host.VersionBefore": "a version", "Host.VersionAfter": "a version", "Host.Update": "a state", "Host.On": "a state",
		"Host.Arm": "a state", "Host.Confirm": "a state", "Host.To": "a state",
	}
	const addr = "owner@example.com"
	filled := map[string]bool{}
	var fill func(v reflect.Value)
	fill = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer:
			if v.Type().Elem().Kind() == reflect.Struct && v.Type().Elem().PkgPath() == reflect.TypeOf(wakerun.Run{}).PkgPath() {
				v.Set(reflect.New(v.Type().Elem()))
				fill(v.Elem())
			}
		case reflect.Slice:
			if k := v.Type().Elem().Kind(); k == reflect.Struct || k == reflect.Pointer {
				v.Set(reflect.MakeSlice(v.Type(), 1, 1))
				fill(v.Index(0))
			}
		case reflect.Struct:
			if v.Type().PkgPath() != reflect.TypeOf(wakerun.Run{}).PkgPath() {
				return // a time
			}
			for i := 0; i < v.NumField(); i++ {
				f := v.Type().Field(i)
				name := v.Type().Name() + "." + f.Name
				if !f.IsExported() {
					continue
				}
				if f.Type.Kind() == reflect.String {
					if never[name] == "" {
						v.Field(i).SetString("x " + addr + " (unverified)")
						filled[name] = true
					}
					continue
				}
				fill(v.Field(i))
			}
		}
	}
	r := &wakerun.Run{}
	fill(reflect.ValueOf(r).Elem())
	for _, must := range []string{"Run.By", "Run.ByActor", "Run.Tried", "Run.Trying", "Run.ConfirmedBy", "Run.Note", "Run.Summary", "Cancel.By",
		"Host.Text", "Host.UpdateText", "Host.ConfirmTried", "LogLine.Text"} {
		if !filled[must] {
			t.Fatalf("%s was not filled: the check does not walk the run any more (%d fields filled)", must, len(filled))
		}
	}
	d := access.Data{Users: map[string]access.User{"sub-n": {Sub: "sub-n", Username: "Juniper", Email: addr}}}
	for _, who := range []*access.People{d.People(), access.Data{}.People()} { // the account is known, and it is not
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var c wakerun.Run
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(runShown(who, &c))
		if err != nil {
			t.Fatal(err)
		}
		var texts map[string]any
		_ = json.Unmarshal(out, &texts)
		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch x := v.(type) {
			case string:
				if strings.Contains(x, "@") || strings.Contains(x, "unverified") {
					t.Errorf("a run as the page gets it: %s is %q", path, x)
				}
			case map[string]any:
				for k, e := range x {
					walk(path+"."+k, e)
				}
			case []any:
				for _, e := range x {
					walk(path+"[]", e)
				}
			}
		}
		walk("run", texts)
		if strings.Contains(string(out), "by_actor") {
			t.Errorf("a run as the page gets it has the audit log's text of who started it: %s", clipText(string(out), 300))
		}
	}
}

// Somebody the panel has no account for (users.json cannot be read, or the account was removed a moment ago and
// the request was already on its way) is not named by the address either: a sentence, an agent and a
// notification get the words that stand for it. A record keeps what it kept before, and that is hidden where
// the record is handed out. An API token is named by its name and its owner's user name.
func TestSomebodyThePanelDoesNotKnowIsNotNamedByTheAddress(t *testing.T) {
	e := setup(t)
	if _, _, err := e.s.Access.SeenIdentity(access.Identity{Sub: "sub-n", Email: "owner@example.com", EmailVerified: true, Name: "Real Name", Username: "Juniper"}, access.RoleAdmin, "test"); err != nil {
		t.Fatal(err)
	}
	ghost := &model.User{Sub: "nobody-knows", Email: model.P("ghost@example.com"), EmailVerified: true, Name: model.P("Ghost Person")}
	if got := e.s.whoShown(ghost); got != access.HiddenAddress {
		t.Errorf("somebody without an account is named %q in a sentence", got)
	}
	if kept := e.s.who(ghost); kept != access.HiddenAddress {
		t.Errorf("a record of somebody without an account keeps %q", kept)
	}
	for _, named := range []*model.User{{Sub: "nobody-knows-2", Name: model.P("Ghost Person")}, {Sub: "nobody-knows-3", PreferredUsername: model.P("ghost")}} {
		if got, kept := e.s.whoShown(named), e.s.who(named); got != access.HiddenName || kept != access.HiddenName {
			t.Errorf("somebody without an account and without an address is named %q, and a record keeps %q", got, kept)
		}
	}
	if got := e.s.whoShown(&model.User{Sub: "nobody-knows-4"}) + "|" + e.s.who(&model.User{Sub: "nobody-knows-4"}); got != "nobody-knows-4|nobody-knows-4" {
		t.Errorf("somebody with nothing but an id: %q", got)
	}
	// (an account the panel knows, without a user name: a record keeps what it kept before, hidden where it is handed out)
	if _, _, err := e.s.Access.SeenIdentity(access.Identity{Sub: "sub-o", Email: "old@example.com", EmailVerified: true, Name: "Old Timer"}, access.RoleViewer, "test"); err != nil {
		t.Fatal(err)
	}
	old := &model.User{Sub: "sub-o", Email: model.P("old@example.com"), EmailVerified: true, Name: model.P("Old Timer")}
	if kept := e.s.who(old); kept != "old@example.com" || e.s.shown(kept) != access.HiddenAddress || e.s.whoShown(old) != access.HiddenAddress {
		t.Errorf("a record of an account without a user name keeps %q, handed out as %q; in a sentence %q", kept, e.s.shown(kept), e.s.whoShown(old))
	}
	token := &model.User{Sub: "token:ci", Auth: "token", TokenName: model.P("ci"), TokenOwner: model.P("sub-n")}
	if got := e.s.whoShown(token); got != "ci (token, owner Juniper)" {
		t.Errorf("a token is named %q in a sentence", got)
	}
	if got := e.s.who(token); got != "ci (token, owner sub-n)" || e.s.shown(got) != "ci (token, owner Juniper)" {
		t.Errorf("a record of a token keeps %q, handed out as %q", got, e.s.shown(got))
	}
	stray := &model.User{Sub: "token:x", Auth: "token", TokenName: model.P("deploy"), TokenOwner: model.P("ghost@example.com")}
	if got := e.s.whoShown(stray); got != "deploy (token, owner "+access.HiddenAddress+")" {
		t.Errorf("a token whose owner is written as an address is named %q", got)
	}
}
