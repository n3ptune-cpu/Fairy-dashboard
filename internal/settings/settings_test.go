package settings

import "testing"

func TestSniffAndValidate(t *testing.T) {
	cases := map[string][]byte{"image/png": []byte("\x89PNG\r\n\x1a\nxxxx"), "image/jpeg": []byte("\xff\xd8\xff\xe0"),
		"image/webp": []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/gif": []byte("GIF89a"), "image/avif": []byte("\x00\x00\x00\x1cftypavif")}
	for want, data := range cases {
		if Sniff(data) != want {
			t.Errorf("%s", want)
		}
		if got, err := ValidateUpload(want+"; charset=binary", data); err != nil || got != want {
			t.Errorf("%s: %v", want, err)
		}
	}
	if _, err := ValidateUpload("image/svg+xml", []byte("<svg/>")); err == nil {
		t.Fatal("svg")
	}
	if _, err := ValidateUpload("image/png", nil); err == nil {
		t.Fatal("empty")
	}
	s, err := Decode([]byte(`{"theme":"black","background":{"kind":"url","url":"https://img.example/x.jpg"},"dim":0,"blur":40,"card_opacity":20}`))
	if err != nil || s.Dim != 0 {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"dim":91}`, `{"theme":"white"}`, `{"background":{"kind":"url","url":"javascript:x"}}`, `{"extra":1}`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
