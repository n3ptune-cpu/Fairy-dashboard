package normalise

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

type osCase struct{ kind, platform, pretty, key, name string }

func checkOSIcons(t *testing.T, cases []osCase) {
	t.Helper()
	for _, c := range cases {
		got := OSIcon(c.kind, c.platform, c.pretty)
		switch {
		case c.key == "" && got != nil:
			t.Errorf("%s / %q / %q: got %+v, want no mark", c.kind, c.platform, c.pretty, *got)
		case c.key != "" && (got == nil || got.Key != c.key || got.Name != c.name):
			t.Errorf("%s / %q / %q: got %+v, want %s (%s)", c.kind, c.platform, c.pretty, got, c.key, c.name)
		}
	}
}

// Windows 10 or 11: the name Fairy-agent sends (it already turns the registry's "Windows 10" into "Windows 11"
// from build 22000 on), a build number when the name has one, and what only Windows 11 has (23H2 and later,
// LTSC 2024). Windows Server by its year. Anything else Windows: "windows".
func TestOSIconWindows(t *testing.T) {
	checkOSIcons(t, []osCase{
		// what the agent sends (ProductName + DisplayVersion)
		{"agent", "Windows", "Windows 11 Home 24H2", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 11 Enterprise 24H2", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 11 Pro 22H2", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 11 Pro 21H2", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 10 Pro 22H2", "windows10", "Windows 10"},
		{"agent", "Windows", "Windows 10 Home 21H2", "windows10", "Windows 10"},
		{"agent", "Windows", "Windows 10 Pro 2009", "windows10", "Windows 10"}, // ReleaseId, no DisplayVersion
		{"agent", "Windows", "Windows 10 Pro", "windows10", "Windows 10"},
		// LTSC and IoT editions
		{"agent", "Windows", "Windows 11 IoT Enterprise LTSC 24H2", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 11 IoT Enterprise LTSC 2024 24H2", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 11 Enterprise LTSC 2024", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 10 Enterprise LTSC 2021", "windows10", "Windows 10"}, // build 19044
		{"agent", "Windows", "Windows 10 Enterprise LTSC 2021 21H2", "windows10", "Windows 10"},
		{"agent", "Windows", "Windows 10 Enterprise LTSC 2019 1809", "windows10", "Windows 10"},
		{"agent", "Windows", "Windows 10 IoT Enterprise LTSC 2021 21H2", "windows10", "Windows 10"},
		{"agent", "Windows", "Windows 10 Enterprise 2016 LTSB 1607", "windows10", "Windows 10"},
		// the registry's name, not put right (another agent): what only Windows 11 has decides
		{"agent", "Windows", "Windows 10 Pro 24H2", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 10 Home 23H2", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 10 Pro 25H2", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 10 IoT Enterprise LTSC 2024", "windows11", "Windows 11"},
		// a build number in the name wins over the name: 22000 and later is Windows 11
		{"agent", "Windows", "Windows 10 Pro 10.0.22631", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 10 Pro 10.0.22000", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 10 Pro 10.0.21999", "windows10", "Windows 10"},
		{"agent", "Windows", "Windows 10 Pro 10.0.19045", "windows10", "Windows 10"},
		{"agent", "Windows", "Windows 10 Enterprise LTSC 2021 (build 19044)", "windows10", "Windows 10"},
		{"agent", "Windows", "Windows 10 Pro (Build 26100.2314)", "windows11", "Windows 11"},
		{"agent", "Windows", "Microsoft Windows [Version 10.0.19045.5131]", "windows10", "Windows 10"},
		{"agent", "Windows", "Microsoft Windows [Version 10.0.26100.2314]", "windows11", "Windows 11"},
		{"agent", "Windows", "Windows 10.0.10240", "windows10", "Windows 10"},
		{"agent", "Windows", "Windows 6.3 build 9600", "windows", "Windows"},
		// Windows Server: 2016, 2019 and 2022 are Windows 10 underneath, 2025 is Windows 11
		{"agent", "Windows", "Windows Server 2025 Datacenter 24H2", "windows11", "Windows Server 2025"},
		{"agent", "Windows", "Windows Server 2025 Standard", "windows11", "Windows Server 2025"},
		{"agent", "Windows", "Windows Server 2022 Standard 21H2", "windows10", "Windows Server 2022"},
		{"agent", "Windows", "Windows Server 2019 Datacenter 1809", "windows10", "Windows Server 2019"},
		{"agent", "Windows", "Windows Server 2016 Standard 1607", "windows10", "Windows Server 2016"},
		{"agent", "Windows", "Windows Server 2012 R2 Standard", "windows", "Windows Server 2012"},
		{"agent", "Windows", "Windows Server 2008 R2 Enterprise", "windows", "Windows Server 2008"},
		{"agent", "Windows", "Windows Server Standard", "windows", "Windows Server"},
		{"agent", "Windows", "Windows Server Datacenter (build 20348)", "windows10", "Windows Server"},
		{"agent", "Windows", "Windows Server Datacenter 10.0.26100", "windows11", "Windows Server"},
		// not known: the generic Windows mark
		{"agent", "Windows", "Windows", "windows", "Windows"}, // hide_host_details
		{"agent", "Windows", "", "windows", "Windows"},
		{"agent", "Windows", "Windows 8.1 Pro", "windows", "Windows"},
		{"agent", "Windows", "Windows 7 Professional", "windows", "Windows"},
		{"agent", "Windows", "Windows 12 Pro", "windows", "Windows"}, // a version this panel does not know
		{"agent", "windows", "WINDOWS 11 PRO 24H2", "windows11", "Windows 11"},
		{"agent", "", "Windows 10 Pro 22H2", "windows10", "Windows 10"}, // no platform.system: the name alone
		// numbers that are not a version of Windows or a build
		{"agent", "Windows", "Windows 10 Pro for Workstations 22H2", "windows10", "Windows 10"},
		{"agent", "Windows", "Windows 10 Enterprise N LTSC 2021", "windows10", "Windows 10"},
		{"agent", "Windows", "Windows 10 Pro 99999999999999999999", "windows10", "Windows 10"},
		// the platform decides, not a word in the name
		{"agent", "Windows", "Ubuntu 24.04 LTS", "windows", "Windows"},
		{"agent", "Linux", "Windows", "linux", "Linux"},
	})
}

func TestOSIconProxmoxAndLinux(t *testing.T) {
	checkOSIcons(t, []osCase{
		// a Proxmox node, whatever else is said; an agent on a Proxmox host says "Proxmox VE 9.0"
		{"node", "", "", "proxmox", "Proxmox VE"},
		{"node", "Linux", "Debian GNU/Linux 13 (trixie)", "proxmox", "Proxmox VE"},
		{"agent", "Linux", "Proxmox VE 9.0", "proxmox", "Proxmox VE"},
		{"agent", "Linux", "Proxmox VE 8.2 (Debian GNU/Linux 12)", "proxmox", "Proxmox VE"},
		{"agent", "Linux", "Proxmox Backup Server 3.2", "proxmox", "Proxmox"},
		// each distribution by its PRETTY_NAME
		{"agent", "Linux", "Ubuntu 24.04.1 LTS", "ubuntu", "Ubuntu"},
		{"agent", "Linux", "Ubuntu Core 22", "ubuntu", "Ubuntu"},
		{"agent", "Linux", "Debian GNU/Linux 13 (trixie)", "debian", "Debian"},
		{"agent", "Linux", "Debian GNU/Linux trixie/sid", "debian", "Debian"},
		{"agent", "Linux", "Fedora Linux 41 (Workstation Edition)", "fedora", "Fedora"},
		{"agent", "Linux", "Arch Linux", "arch", "Arch Linux"},
		{"agent", "Linux", "Arch Linux ARM", "arch", "Arch Linux"},
		{"agent", "Linux", "Alpine Linux v3.20", "alpine", "Alpine Linux"},
		{"agent", "Linux", "openSUSE Leap 15.6", "opensuse", "openSUSE"},
		{"agent", "Linux", "openSUSE Tumbleweed", "opensuse", "openSUSE"},
		{"agent", "Linux", "Rocky Linux 9.4 (Blue Onyx)", "rocky", "Rocky Linux"},
		{"agent", "Linux", "AlmaLinux 9.4 (Seafoam Ocelot)", "almalinux", "AlmaLinux"},
		{"agent", "Linux", "CentOS Stream 9", "centos", "CentOS"},
		{"agent", "Linux", "CentOS Linux 7 (Core)", "centos", "CentOS"},
		{"agent", "Linux", "Red Hat Enterprise Linux 9.4 (Plow)", "redhat", "Red Hat Enterprise Linux"},
		{"agent", "Linux", "RHEL 9.4", "redhat", "Red Hat Enterprise Linux"},
		{"agent", "Linux", "Linux Mint 22", "linuxmint", "Linux Mint"},
		{"agent", "Linux", "LMDE 6 (faye)", "linuxmint", "Linux Mint"},
		{"agent", "Linux", "NixOS 24.05 (Uakari)", "nixos", "NixOS"},
		{"agent", "Linux", "Raspbian GNU/Linux 12 (bookworm)", "raspberrypi", "Raspberry Pi OS"},
		{"agent", "Linux", "Raspberry Pi OS (64-bit)", "raspberrypi", "Raspberry Pi OS"},
		{"agent", "Linux", "Manjaro Linux", "manjaro", "Manjaro"},
		{"agent", "Linux", "Kali GNU/Linux Rolling", "kali", "Kali Linux"},
		{"agent", "Linux", "Gentoo Linux", "gentoo", "Gentoo"},
		{"agent", "Linux", "Gentoo/Linux", "gentoo", "Gentoo"},
		{"agent", "Linux", "Void Linux", "void", "Void Linux"},
		{"agent", "Linux", "Unraid OS 7.0 x86_64", "unraid", "Unraid"},
		{"agent", "Linux", "TrueNAS SCALE 24.10", "truenas", "TrueNAS"},
		{"agent", "freebsd", "TrueNAS CORE 13.0", "truenas", "TrueNAS"},
		{"agent", "freebsd", "freebsd", "freebsd", "FreeBSD"}, // an agent built for another system sends Go's name twice
		{"agent", "", "FreeBSD 14.1-RELEASE", "freebsd", "FreeBSD"},
		// case does not matter, punctuation separates words
		{"agent", "linux", "UBUNTU 24.04.1 LTS", "ubuntu", "Ubuntu"},
		{"agent", "Linux", "debian-13", "debian", "Debian"},
		{"agent", "", "Ubuntu 22.04.5 LTS", "ubuntu", "Ubuntu"}, // no platform.system: the name alone
	})
}

// Names that look like another system's: whole words are compared, and the more specific system comes first.
func TestOSIconLookAlikes(t *testing.T) {
	checkOSIcons(t, []osCase{
		// "ubuntu" inside another word is not Ubuntu by accident; the three flavours are Ubuntu on purpose
		{"agent", "Linux", "Kubuntu 24.04", "ubuntu", "Ubuntu"},
		{"agent", "Linux", "Xubuntu 24.04", "ubuntu", "Ubuntu"},
		{"agent", "Linux", "Lubuntu 24.04", "ubuntu", "Ubuntu"},
		{"agent", "Linux", "Fubuntu 1.0", "linux", "Linux"},
		{"agent", "Linux", "NotUbuntuAtAll", "linux", "Linux"},
		// "Debian" in Raspbian, Proxmox, Kali, Mint: the system named wins over the one it is built on
		{"agent", "Linux", "Raspbian GNU/Linux 12 (bookworm)", "raspberrypi", "Raspberry Pi OS"},
		{"agent", "Linux", "Raspberry Pi OS (Debian GNU/Linux 12)", "raspberrypi", "Raspberry Pi OS"},
		{"agent", "Linux", "Proxmox VE 9.0 (Debian 13)", "proxmox", "Proxmox VE"},
		{"agent", "Linux", "Kali GNU/Linux Rolling (Debian testing)", "kali", "Kali Linux"},
		{"agent", "Linux", "Linux Mint Debian Edition 6", "linuxmint", "Linux Mint"},
		{"agent", "Linux", "Linux Mint 22 (Ubuntu noble)", "linuxmint", "Linux Mint"},
		{"agent", "Linux", "TrueNAS SCALE (Debian GNU/Linux 12)", "truenas", "TrueNAS"},
		{"agent", "Linux", "Devuan GNU/Linux 5 (daedalus)", "linux", "Linux"},
		{"agent", "Linux", "Armbian 24.5 bookworm", "linux", "Linux"},
		// Arch and its relatives
		{"agent", "Linux", "Manjaro Linux (Arch Linux)", "manjaro", "Manjaro"},
		{"agent", "Linux", "Archcraft", "linux", "Linux"},
		{"agent", "Linux", "EndeavourOS Linux", "linux", "Linux"},
		{"agent", "Linux", "Parch Linux", "linux", "Linux"},
		{"agent", "Linux", "Linux aarch64", "linux", "Linux"},
		// Red Hat's family
		{"agent", "Linux", "Oracle Linux Server 9.4", "linux", "Linux"},
		{"agent", "Linux", "Amazon Linux 2023", "linux", "Linux"},
		{"agent", "Linux", "Red Star OS", "linux", "Linux"},                         // "red" without "hat"
		{"agent", "Linux", "SUSE Linux Enterprise Server 15 SP6", "linux", "Linux"}, // not openSUSE
		// other distributions: the penguin
		{"agent", "Linux", "Pop!_OS 24.04 LTS", "linux", "Linux"},
		{"agent", "Linux", "Zorin OS 17.2", "linux", "Linux"},
		{"agent", "Linux", "Linux", "linux", "Linux"}, // hide_host_details
		{"agent", "Linux", "", "linux", "Linux"},
		{"agent", "", "Slackware Linux 15.0", "linux", "Linux"},
		{"agent", "Linux", "Unraided", "linux", "Linux"},
		{"agent", "Linux", "Avoid Linux", "linux", "Linux"},
	})
}

// macOS gets a plain laptop (key "macos"), never Apple's mark; a system nobody knows gets no mark at all, and
// the page keeps its plain device glyph. Guests have no known system.
func TestOSIconOtherAndUnknown(t *testing.T) {
	checkOSIcons(t, []osCase{
		{"agent", "darwin", "darwin", "macos", "macOS"},
		{"agent", "", "macOS 15.1 Sequoia", "macos", "macOS"},
		{"agent", "", "Mac OS X 10.15.7", "macos", "macOS"},
		{"agent", "", "", "", ""},
		{"agent", "openbsd", "openbsd", "", ""},
		{"agent", "", "Haiku R1/beta5", "", ""},
		{"agent", "plan9", "plan9", "", ""},
		{"qemu", "", "", "", ""},
		{"lxc", "", "", "", ""},
	})
}

// Every key OSIcon can answer with is in OSIconKeys (the page has a mark for each of those), and each of them
// is reached by a name a real system sends.
func TestOSIconKeys(t *testing.T) {
	sample := map[string][3]string{
		"windows11": {"agent", "Windows", "Windows 11 Pro 24H2"}, "windows10": {"agent", "Windows", "Windows 10 Pro 22H2"},
		"windows": {"agent", "Windows", "Windows"}, "macos": {"agent", "darwin", "darwin"}, "proxmox": {"node", "", ""},
		"ubuntu": {"agent", "Linux", "Ubuntu 24.04.1 LTS"}, "debian": {"agent", "Linux", "Debian GNU/Linux 13 (trixie)"},
		"fedora": {"agent", "Linux", "Fedora Linux 41 (Server Edition)"}, "arch": {"agent", "Linux", "Arch Linux"},
		"alpine": {"agent", "Linux", "Alpine Linux v3.20"}, "opensuse": {"agent", "Linux", "openSUSE Leap 15.6"},
		"rocky": {"agent", "Linux", "Rocky Linux 9.4 (Blue Onyx)"}, "almalinux": {"agent", "Linux", "AlmaLinux 9.4 (Seafoam Ocelot)"},
		"centos": {"agent", "Linux", "CentOS Stream 9"}, "redhat": {"agent", "Linux", "Red Hat Enterprise Linux 9.4 (Plow)"},
		"linuxmint": {"agent", "Linux", "Linux Mint 22"}, "nixos": {"agent", "Linux", "NixOS 24.05 (Uakari)"},
		"raspberrypi": {"agent", "Linux", "Raspbian GNU/Linux 12 (bookworm)"}, "manjaro": {"agent", "Linux", "Manjaro Linux"},
		"kali": {"agent", "Linux", "Kali GNU/Linux Rolling"}, "gentoo": {"agent", "Linux", "Gentoo Linux"},
		"void": {"agent", "Linux", "Void Linux"}, "unraid": {"agent", "Linux", "Unraid OS 7.0 x86_64"},
		"truenas": {"agent", "Linux", "TrueNAS SCALE 24.10"}, "freebsd": {"agent", "freebsd", "freebsd"},
		"linux": {"agent", "Linux", "Pop!_OS 24.04 LTS"},
	}
	if len(sample) != len(OSIconKeys) {
		t.Fatalf("%d samples for %d keys", len(sample), len(OSIconKeys))
	}
	for _, k := range OSIconKeys {
		in, ok := sample[k]
		if got := OSIcon(in[0], in[1], in[2]); !ok || got == nil || got.Key != k || got.Name == "" {
			t.Errorf("%s: %v gives %+v", k, in, got)
		}
	}
	// whatever a host says, the answer is nothing or one of the keys, with a name
	for _, kind := range []string{"agent", "node", "qemu", ""} {
		for _, sys := range []string{"", "Windows", "Linux", "darwin", "freebsd", "x"} {
			for _, pretty := range []string{"", "Windows", "Windows Server", "Windows Server 99999", "Windows 10.0.x", "build", "10 0",
				"Linux", "<svg onload=alert(1)>", "windows11", "../../proxmox", "Ubuntu\x00Debian", "ltsc", "server 2025 build 1"} {
				if got := OSIcon(kind, sys, pretty); got != nil && (!slices.Contains(OSIconKeys, got.Key) || got.Name == "" || strings.ContainsAny(got.Name, "<>&\"'")) {
					t.Errorf("%q / %q / %q: %+v", kind, sys, pretty, got)
				}
			}
		}
	}
}

// The normalisers fill os_icon: an agent from its platform block, a Proxmox node always, a guest never.
func TestOSIconInSystems(t *testing.T) {
	now := time.Now()
	agent := func(system, pretty string) *model.OSIcon {
		m := jsonx.M{"hostname": "h", "platform": jsonx.M{"system": system, "pretty": pretty}}
		return NormaliseAgent(config.Agent{ID: "a"}, m, now, 1, NewRateTracker()).OSIcon
	}
	if i := agent("Windows", "Windows 11 Home 24H2"); i == nil || *i != (model.OSIcon{Key: "windows11", Name: "Windows 11"}) {
		t.Errorf("agent, Windows 11: %+v", i)
	}
	if i := agent("Linux", "Ubuntu 24.04.1 LTS"); i == nil || *i != (model.OSIcon{Key: "ubuntu", Name: "Ubuntu"}) {
		t.Errorf("agent, Ubuntu: %+v", i)
	}
	if i := NormaliseAgent(config.Agent{ID: "a"}, jsonx.M{"hostname": "h"}, now, 1, NewRateTracker()).OSIcon; i != nil {
		t.Errorf("agent without a platform block: %+v", i)
	}
	res := []jsonx.M{{"type": "node", "node": "pve1", "status": "online"},
		{"type": "qemu", "vmid": 100.0, "name": "vm", "node": "pve1", "status": "running"}}
	for _, s := range NormaliseProxmox(res, now, 1, NewRateTracker(), ProxmoxOpts{}) {
		switch {
		case s.Kind == "node" && (s.OSIcon == nil || *s.OSIcon != (model.OSIcon{Key: "proxmox", Name: "Proxmox VE"})):
			t.Errorf("node: %+v", s.OSIcon)
		case s.Kind != "node" && s.OSIcon != nil:
			t.Errorf("guest: %+v", s.OSIcon)
		}
	}
}
