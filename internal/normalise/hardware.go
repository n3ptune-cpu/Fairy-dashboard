package normalise

import (
	"fmt"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// Placeholders are SMBIOS strings that board vendors leave unfilled (compared case-insensitively, trimmed).
var Placeholders = map[string]bool{"": true, "system product name": true, "system manufacturer": true,
	"to be filled by o.e.m.": true, "to be filled by oem": true, "default string": true, "o.e.m.": true, "oem": true,
	"not applicable": true, "none": true, "n/a": true, "na": true, "unknown": true, "system version": true,
	"type1productconfigid": true, "not specified": true, "base board product name": true,
	"base board manufacturer": true, "all series": true}

var vendorShort = map[string]string{"asustek computer inc.": "ASUS", "asustek computer inc": "ASUS",
	"supermicro": "Supermicro", "super micro computer, inc.": "Supermicro", "micro-star international co., ltd.": "MSI",
	"gigabyte technology co., ltd.": "Gigabyte", "asrock": "ASRock", "dell inc.": "Dell", "lenovo": "Lenovo",
	"hp": "HP", "hewlett-packard": "HP"}

func realStr(v *string) *string {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(*v)
	if Placeholders[strings.ToLower(s)] {
		return nil
	}
	return &s
}

func trimmed(m jsonx.M, k string) *string {
	v, ok := m[k]
	if !ok || v == nil {
		return nil
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
	default:
		s = fmt.Sprint(x)
	}
	s = ClipText(strings.TrimSpace(s), maxLong)
	return &s
}

// HardwareDisplay builds the hardware block and its display name: system vendor + model, falling back to the
// mainboard when the model is a placeholder ("System Product Name", "To be filled by O.E.M." ...), e.g. a
// Supermicro X11SCA-F in a custom build.
func HardwareDisplay(hw jsonx.M) *model.Hardware {
	if len(hw) == 0 {
		return nil
	}
	h := &model.Hardware{SystemVendor: trimmed(hw, "system_vendor"), SystemModel: trimmed(hw, "system_model"),
		BoardVendor: trimmed(hw, "board_vendor"), BoardModel: trimmed(hw, "board_model"),
		BIOSVersion: trimmed(hw, "bios_version")}
	mdl, board := realStr(h.SystemModel), realStr(h.BoardModel)
	var vendor *string
	if mdl != nil {
		vendor = realStr(h.SystemVendor)
	} else if board != nil {
		vendor, mdl = realStr(h.BoardVendor), board
	}
	if mdl != nil {
		short := ""
		if vendor != nil {
			short = *vendor
			if v, ok := vendorShort[strings.ToLower(*vendor)]; ok {
				short = v
			}
		}
		d := *mdl
		if short != "" && !strings.HasPrefix(strings.ToLower(d), strings.ToLower(short)) {
			d = short + " " + d
		}
		h.Display = &d
	}
	return h
}
