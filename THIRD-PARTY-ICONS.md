# Third-party icons

The panel shows, next to a host, the mark of the system it runs (Windows, Proxmox VE, a Linux distribution).
All of them are in one file, `internal/web/static/osicons.js`: one SVG path per mark on a 24 x 24 grid, drawn
by the page in the text colour. The file holds path data only: no colours, no scripts, no addresses. Nothing is
loaded from another server.

The marks and names are trademarks of their owners. Fairy uses them only to show which system a host runs; it
is not affiliated with, sponsored by or endorsed by any of them. If you are such an owner and want a mark
changed or removed, open an issue.

## Drawn for Fairy

Plain shapes, drawn by hand for this project and covered by its [LICENSE](LICENSE) (MIT). They are not copied
from a file of Microsoft or Apple, but the Windows ones are meant to be recognised as Windows, and the
Windows logos are trademarks of Microsoft.

| Key | Shown for | What it is |
|---|---|---|
| `windows11` | Windows 11, Windows Server 2025 and later | four equal squares in a 2 x 2 grid |
| `windows10` | Windows 10, Windows Server 2016, 2019 and 2022 | the same four panes, slanted |
| `windows` | a Windows whose version is not known | the same path as `windows11` |
| `macos` | macOS | a plain laptop. Apple's mark is not used |

## From Simple Icons

Source: [Simple Icons](https://github.com/simple-icons/simple-icons), version 16.37.0 (commit
`cd796c98565fc466923fc1128b962e133fa3c612`). For each mark the `d` attribute of the single `<path>` of the
file below was copied without a change; nothing else of the file is used.

Simple Icons as a collection is released under CC0 1.0 Universal. Its `DISCLAIMER.md` says that this does not
mean every icon in it is CC0, that a missing licence entry does not mean an icon has no licence, and that
users should check each brand's own terms. The table has what its `data/simple-icons.json` records for each
icon: a licence of the icon itself, the brand's guidelines, and where the icon was taken from.

| Key | Shown for | File in Simple Icons | Licence of the icon (as recorded) | Brand guidelines (as recorded) | Icon taken from (as recorded) |
|---|---|---|---|---|---|
| `proxmox` | Proxmox VE | `icons/proxmox.svg` ("Proxmox") | none recorded | https://www.proxmox.com/en/news/media-kit | https://www.proxmox.com/en/news/media-kit |
| `ubuntu` | Ubuntu | `icons/ubuntu.svg` ("Ubuntu") | none recorded | https://design.ubuntu.com/brand | https://design.ubuntu.com/resources |
| `debian` | Debian | `icons/debian.svg` ("Debian") | CC-BY-SA-3.0 | https://www.debian.org/logos/ | https://www.debian.org/logos |
| `fedora` | Fedora | `icons/fedora.svg` ("Fedora") | custom (https://docs.fedoraproject.org/en-US/project/brand/) | https://fedoraproject.org/wiki/Legal:Trademark_guidelines | https://docs.fedoraproject.org/en-US/project/brand/ |
| `arch` | Arch Linux | `icons/archlinux.svg` ("Arch Linux") | none recorded | https://wiki.archlinux.org/index.php/DeveloperWiki:TrademarkPolicy#Logo_Usage_Guidelines | https://www.archlinux.org/art |
| `alpine` | Alpine Linux | `icons/alpinelinux.svg` ("Alpine Linux") | none recorded | none recorded | https://alpinelinux.org |
| `opensuse` | openSUSE | `icons/opensuse.svg` ("openSUSE") | none recorded | https://en.opensuse.org/Portal:Artwork | https://github.com/openSUSE/artwork/blob/33e94aa76837c09f03d1712705949b71a246a53b/logos/buttons/button-colour.svg |
| `rocky` | Rocky Linux | `icons/rockylinux.svg` ("Rocky Linux") | CC-BY-SA-4.0 | none recorded | https://github.com/rocky-linux/branding/blob/94e97dd30b87d909cc4f6a6838a2926f77f9ac47/logo/src/icon-black.svg |
| `almalinux` | AlmaLinux | `icons/almalinux.svg` ("AlmaLinux") | none recorded | https://almalinux.org/p/the-almalinux-os-trademark-usage-policy/ | https://almalinux.org |
| `centos` | CentOS | `icons/centos.svg` ("CentOS") | none recorded | none recorded | https://wiki.centos.org/ArtWork/Brand/Logo |
| `redhat` | Red Hat Enterprise Linux | `icons/redhat.svg` ("Red Hat") | none recorded | none recorded | https://www.redhat.com/en/about/brand/new-brand/details |
| `linuxmint` | Linux Mint | `icons/linuxmint.svg` ("Linux Mint") | none recorded | none recorded | https://github.com/linuxmint/brand-logo/blob/540ac3b08e987866d77a340f557f994c988ac2ae/ring-mono-green.svg |
| `nixos` | NixOS | `icons/nixos.svg` ("NixOS") | CC-BY-4.0 | https://brand.nixos.org/documents/nixos-branding-guide.pdf | https://brand.nixos.org/logos/nixos-logomark-black-flat-minimal.svg |
| `raspberrypi` | Raspberry Pi OS | `icons/raspberrypi.svg` ("Raspberry Pi") | none recorded | https://www.raspberrypi.org/trademark-rules | https://www.raspberrypi.org/trademark-rules |
| `manjaro` | Manjaro | `icons/manjaro.svg` ("Manjaro") | none recorded | none recorded | https://manjaro.org |
| `kali` | Kali Linux | `icons/kalilinux.svg` ("Kali Linux") | none recorded | https://www.kali.org/docs/policy/trademark/ | https://www.kali.org/docs/policy/trademark/ |
| `gentoo` | Gentoo | `icons/gentoo.svg` ("Gentoo") | CC-BY-SA-2.5 | https://www.gentoo.org/inside-gentoo/foundation/name-logo-guidelines.html | https://wiki.gentoo.org/wiki/Project:Artwork/Artwork#Variations_of_the_.22g.22_logo |
| `void` | Void Linux | `icons/voidlinux.svg` ("Void Linux") | none recorded | none recorded | https://alpha.de.repo.voidlinux.org/logos/void-dark2.svg |
| `unraid` | Unraid | `icons/unraid.svg` ("Unraid") | none recorded | none recorded | https://unraid.net |
| `truenas` | TrueNAS | `icons/truenas.svg` ("TrueNAS") | none recorded | none recorded | https://www.truenas.com |
| `freebsd` | FreeBSD | `icons/freebsd.svg` ("FreeBSD") | none recorded | none recorded | https://www.freebsdfoundation.org/about/project/ |
| `linux` | Linux (any other distribution): Tux | `icons/linux.svg` ("Linux") | none recorded | none recorded | https://www.linuxfoundation.org/the-linux-mark/ |

Notes on the icons with their own licence:

* Debian: CC BY-SA 3.0. The Debian "open use" logo, by the Debian project.
* Rocky Linux: CC BY-SA 4.0. The Rocky Linux icon, by the Rocky Linux project.
* Gentoo: CC BY-SA 2.5. The Gentoo "g" logo, by the Gentoo project.
* NixOS: CC BY 4.0. The NixOS logomark, by the NixOS project.
* Fedora: "custom": the terms on the Fedora project's brand page and in its trademark guidelines.

These paths stay under those licences; they are not relicensed under Fairy's MIT licence. The share-alike
licences ask that changed versions of the icon keep the same licence; Fairy does not change the paths.

Tux, the penguin (`linux`), was drawn by Larry Ewing (lewing@isc.tamu.edu) with The GIMP. Linux is a
registered trademark of Linus Torvalds.
