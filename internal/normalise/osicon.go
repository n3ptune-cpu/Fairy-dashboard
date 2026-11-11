package normalise

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// OSIconKeys are the keys OSIcon answers with. static/osicons.js has one mark for each (a test compares the two).
var OSIconKeys = []string{"windows11", "windows10", "windows", "macos", "proxmox", "ubuntu", "debian", "fedora", "arch",
	"alpine", "opensuse", "rocky", "almalinux", "centos", "redhat", "linuxmint", "nixos", "raspberrypi", "manjaro", "kali",
	"gentoo", "void", "unraid", "truenas", "freebsd", "linux"}

// OSIcon says which system mark the page draws for a host, and the system's short name (the mark's accessible
// name). It reads only what the panel already has: the kind ("node" is a Proxmox node), the agent's
// platform.system ("Linux", "Windows", or Go's name of another system: "darwin", "freebsd") and platform.pretty
// ("Windows 11 Home 24H2", "Ubuntu 24.04.1 LTS", "Proxmox VE 9.0", or only "Windows" / "Linux" from an agent with
// hide_host_details). The agent sends no build number and no os-release ID, so the name is all there is; whole
// words are compared, never parts of words ("Kubuntu" is not read as "Ubuntu" by accident, "Raspbian" is not
// Debian). nil: nothing is known, the page keeps its plain device glyph.
//
// The keys are the marks of static/osicons.js; the page draws its plain glyph for a key it does not have.
func OSIcon(kind, platform, pretty string) *model.OSIcon {
	if kind == "node" { // a Proxmox node (its system underneath is Debian)
		return &model.OSIcon{Key: "proxmox", Name: "Proxmox VE"}
	}
	sys := strings.ToLower(strings.TrimSpace(platform))
	w := strings.FieldsFunc(strings.ToLower(pretty), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	has := func(word string) bool { return slices.Contains(w, word) }
	seq := func(a, b string) bool { // the words a b, next to each other
		for i := 0; i+1 < len(w); i++ {
			if w[i] == a && w[i+1] == b {
				return true
			}
		}
		return false
	}
	icon := func(key, name string) *model.OSIcon { return &model.OSIcon{Key: key, Name: name} }
	switch {
	case sys == "windows" || sys == "" && has("windows"):
		return windowsIcon(w)
	case sys == "darwin" || has("macos") || has("darwin") || seq("mac", "os") || seq("os", "x"):
		return icon("macos", "macOS") // a plain laptop, not Apple's mark
	// Linux (and FreeBSD): the most specific name first. Proxmox, TrueNAS, Raspberry Pi OS, Kali, Mint and Ubuntu
	// come before the systems they are built on.
	case has("proxmox"):
		if seq("proxmox", "ve") {
			return icon("proxmox", "Proxmox VE")
		}
		return icon("proxmox", "Proxmox")
	case has("truenas"):
		return icon("truenas", "TrueNAS")
	case has("unraid"):
		return icon("unraid", "Unraid")
	case has("raspbian") || seq("raspberry", "pi"):
		return icon("raspberrypi", "Raspberry Pi OS")
	case has("kali"):
		return icon("kali", "Kali Linux")
	case seq("linux", "mint") || has("linuxmint") || has("lmde"):
		return icon("linuxmint", "Linux Mint")
	case has("ubuntu") || has("kubuntu") || has("xubuntu") || has("lubuntu"): // the flavours are Ubuntu (ID=ubuntu)
		return icon("ubuntu", "Ubuntu")
	case has("debian"):
		return icon("debian", "Debian")
	case has("fedora"):
		return icon("fedora", "Fedora")
	case has("rocky"):
		return icon("rocky", "Rocky Linux")
	case has("almalinux") || seq("alma", "linux"):
		return icon("almalinux", "AlmaLinux")
	case has("centos"):
		return icon("centos", "CentOS")
	case has("rhel") || has("redhat") || seq("red", "hat"):
		return icon("redhat", "Red Hat Enterprise Linux")
	case has("opensuse"):
		return icon("opensuse", "openSUSE")
	case has("manjaro"):
		return icon("manjaro", "Manjaro")
	case has("archlinux") || seq("arch", "linux"):
		return icon("arch", "Arch Linux")
	case has("alpine"):
		return icon("alpine", "Alpine Linux")
	case has("nixos"):
		return icon("nixos", "NixOS")
	case has("gentoo"):
		return icon("gentoo", "Gentoo")
	case has("void"):
		return icon("void", "Void Linux")
	case sys == "freebsd" || has("freebsd"):
		return icon("freebsd", "FreeBSD")
	case sys == "linux" || has("linux"):
		return icon("linux", "Linux") // any other distribution: the penguin
	}
	return nil
}

// windowsIcon decides between the Windows 10 mark (four slanted panes), the Windows 11 mark (four squares) and
// "windows" (the version is not known: the page draws the Windows 11 mark for it).
//
// Windows 11 calls itself "Windows 10" in the registry and reports version 10.0; only the build (22000 and
// later) tells them apart. Fairy-agent has put that right in the name it sends since its first release, so the
// name normally decides. A build number in the name ("10.0.22631", "build 26100") wins over the name; so do two
// things that only Windows 11 has: a version of 23H2 or later (Windows 10 ended with 22H2) and LTSC 2024 or later.
//
// Windows Server: 2016, 2019 and 2022 are Windows 10 underneath (builds 14393 to 20348), 2025 and later are
// Windows 11 (build 26100); older ones and names without a year get "windows".
func windowsIcon(w []string) *model.OSIcon {
	num := func(s string) int {
		if len(s) > 6 {
			return -1
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return -1
		}
		return n
	}
	build, version, server, year, eleven := -1, "", false, -1, false
	for i, x := range w {
		next := ""
		if i+1 < len(w) {
			next = w[i+1]
		}
		switch {
		case x == "windows" && version == "" && (next == "10" || next == "11"):
			version = next
		case x == "server":
			server = true
			if y := num(next); y >= 2000 && y < 2100 {
				year = y
			}
		case x == "build" && num(next) >= 1000:
			build = num(next)
		case x == "10" && next == "0" && i+2 < len(w) && num(w[i+2]) >= 1000: // 10.0.22631
			build = num(w[i+2])
		case x == "ltsc" && num(next) >= 2024:
			eleven = true
		case len(x) == 4 && x[2] == 'h' && (x[3] == '1' || x[3] == '2') && num(x[:2]) >= 23: // 23H2, 24H2, 25H2 ...
			eleven = true
		}
	}
	if server {
		name := "Windows Server"
		if year > 0 {
			name += " " + strconv.Itoa(year)
		}
		switch {
		case build >= 26100 || build < 0 && year >= 2025:
			return &model.OSIcon{Key: "windows11", Name: name}
		case build >= 14393 || build < 0 && year >= 2016:
			return &model.OSIcon{Key: "windows10", Name: name}
		}
		return &model.OSIcon{Key: "windows", Name: name}
	}
	switch {
	case build >= 22000, build < 0 && (version == "11" || version == "10" && eleven):
		return &model.OSIcon{Key: "windows11", Name: "Windows 11"}
	case build >= 10240, build < 0 && version == "10":
		return &model.OSIcon{Key: "windows10", Name: "Windows 10"}
	}
	return &model.OSIcon{Key: "windows", Name: "Windows"}
}
