package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The pop-up settings in a request: checked like dim / blur / card_opacity (a value outside the limits is
// refused with 422), and what a request does not name is the default.
func TestPopupSettingsAreValidated(t *testing.T) {
	if d := Defaults().Popups; d != (Popups{Enabled: true, Seconds: 5, Level: "critical"}) {
		t.Fatalf("defaults: %+v", d)
	}
	for body, want := range map[string]Popups{
		`{}`:                           {true, 5, "critical"},
		`{"dim":10}`:                   {true, 5, "critical"},
		`{"popups":null}`:              {true, 5, "critical"},
		`{"popups":{}}`:                {true, 5, "critical"},
		`{"popups":{"enabled":false}}`: {false, 5, "critical"},
		`{"popups":{"seconds":3}}`:     {true, 3, "critical"},
		`{"popups":{"seconds":30,"level":"warning"}}`:                 {true, 30, "warning"},
		`{"popups":{"enabled":false,"seconds":12,"level":"warning"}}`: {false, 12, "warning"},
	} {
		s, err := Decode([]byte(body))
		if err != nil || s.Popups != want {
			t.Errorf("%s: %+v, %v; want %+v", body, s.Popups, err, want)
		}
	}
	for _, bad := range []string{
		`{"popups":{"seconds":2}}`, `{"popups":{"seconds":31}}`, `{"popups":{"seconds":0}}`, `{"popups":{"seconds":-5}}`,
		`{"popups":{"seconds":5.5}}`, `{"popups":{"seconds":"5"}}`, `{"popups":{"seconds":99999999999999999999}}`,
		`{"popups":{"level":"info"}}`, `{"popups":{"level":""}}`, `{"popups":{"level":"CRITICAL"}}`, `{"popups":{"level":7}}`,
		`{"popups":{"enabled":"yes"}}`, `{"popups":{"enabled":1}}`, `{"popups":{"extra":1}}`, `{"popups":"on"}`, `{"popups":[]}`,
	} {
		if s, err := Decode([]byte(bad)); err == nil {
			t.Errorf("accepted %s as %+v", bad, s.Popups)
		}
	}
}

// What is in settings.json: settings saved before the pop-ups existed load as they were, with the pop-up
// defaults; a pop-up value outside the limits becomes its default and the rest of that user's settings stays.
func TestSavedSettingsWithoutOrWithBadPopups(t *testing.T) {
	dir := t.TempDir()
	saved := map[string]json.RawMessage{
		"old":      json.RawMessage(`{"theme":"black","background":{"kind":"url","url":"https://img.example/x.jpg","version":3},"dim":35,"blur":8,"card_opacity":90}`),
		"badsecs":  json.RawMessage(`{"theme":"black","background":{"kind":"none","url":null,"version":0},"dim":35,"blur":8,"card_opacity":90,"popups":{"enabled":false,"seconds":900,"level":"warning"}}`),
		"badlevel": json.RawMessage(`{"theme":"black","background":{"kind":"none","url":null,"version":0},"dim":35,"blur":8,"card_opacity":90,"popups":{"enabled":true,"seconds":20,"level":"everything"}}`),
		"null":     json.RawMessage(`{"theme":"black","background":{"kind":"none","url":null,"version":0},"dim":35,"blur":8,"card_opacity":90,"popups":null}`),
		"wrong":    json.RawMessage(`{"theme":"black","background":{"kind":"none","url":null,"version":0},"dim":35,"blur":8,"card_opacity":90,"popups":"yes"}`),
		"baddim":   json.RawMessage(`{"theme":"black","background":{"kind":"none","url":null,"version":0},"dim":350,"blur":8,"card_opacity":90,"popups":{"enabled":false,"seconds":20,"level":"warning"}}`),
	}
	b, _ := json.Marshal(saved)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	st := NewStore(dir)
	for sub, want := range map[string]Popups{
		"old":      {true, 5, "critical"},
		"badsecs":  {false, 5, "warning"},
		"badlevel": {true, 20, "critical"},
		"null":     {true, 5, "critical"},
		"nobody":   {true, 5, "critical"},
	} {
		got := st.Get(sub)
		if got.Popups != want {
			t.Errorf("%s: %+v, want %+v", sub, got.Popups, want)
		}
		if sub != "nobody" && (got.Dim != 35 || got.Blur != 8 || got.CardOpacity != 90) {
			t.Errorf("%s: the other settings were lost: %+v", sub, got)
		}
	}
	if got := st.Get("old"); got.Background.Kind != "url" || got.Background.URL == nil || *got.Background.URL != "https://img.example/x.jpg" || got.Background.Version != 3 {
		t.Errorf("the old settings' background: %+v", got.Background)
	}
	// as before: settings that cannot be read at all, or with another value outside its limits, are the defaults
	for _, sub := range []string{"wrong", "baddim"} {
		if got := st.Get(sub); got != Defaults() {
			t.Errorf("%s: %+v", sub, got)
		}
	}
	// saved and read back; another user's stay
	s, err := Decode([]byte(`{"dim":20,"popups":{"enabled":false,"seconds":9,"level":"warning"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put("old", s); err != nil {
		t.Fatal(err)
	}
	if got := NewStore(dir).Get("old"); got.Popups != (Popups{false, 9, "warning"}) || got.Dim != 20 {
		t.Fatalf("read back: %+v", got)
	}
	if got := NewStore(dir).Get("badlevel"); got.Popups != (Popups{true, 20, "critical"}) {
		t.Fatalf("another user's settings changed: %+v", got)
	}
}
