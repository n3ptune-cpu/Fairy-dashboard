package demo

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/n3ptune-cpu/Fairy/internal/config"
)

const configYAML = `
public_url: %q
offline_after: 30
enable_docs: true
audit_log: %q
data_dir: %q
oidc: {issuer: "https://id.example.invalid", client_id: demo, control_groups: [controllers]}
proxmox:
  - {id: teyvat, id_prefix: "", cluster_name: NSX-TEYVAT, urls: ["https://nsx-kachina.invalid:8006"],
     token_id: "fairy@pve!panel", token_secret_env: DEMO_PVE_TOKEN,
     # separate token with only the FairyPower role (VM.PowerMgmt) -> guest start/shutdown/reboot/stop
     power_token_id: "fairy@pve!power", power_token_secret_env: DEMO_PVE_POWER_TOKEN}
  # Straw's standalone PVE host, reached over Tailscale node sharing with a read-only token he gave Neptune
  - {id: straw, cluster_name: ASUS-i7, standalone: true, group: Straw, owner: straw,
     urls: ["https://asus-i7.straws-tailnet.invalid:8006"], token_id: "fairy@pve!panel", token_secret_env: DEMO_STRAW_PVE_TOKEN}
agents:
  - {id: nsx-cyrene, name: NSX-CYRENE, url: "http://100.64.0.10:9101", token_env: DEMO_TOKEN, group: Neptune, owner: neptune,
     tags: [server, supermicro], dwservice_url: "https://www.dwservice.net/en/login.html?loginUser=demo-cyrene",
     fan_control: {vendor: supermicro, control_token_env: DEMO_CONTROL_TOKEN}, power_control: true}
  - {id: neptune-laptop, name: "ASUS Vivobook 16X", url: "http://100.64.0.11:9101", token_env: DEMO_TOKEN,
     group: Neptune, owner: neptune, tags: [laptop], power_control: true, control_token_env: DEMO_CONTROL_TOKEN,
     dwservice_url: "https://www.dwservice.net/en/login.html?loginUser=demo-laptop"}
  - {id: llm-box, name: llm-box, url: "http://100.64.0.12:9101", token_env: DEMO_TOKEN, group: Neptune, owner: neptune, tags: [llm]}
  # agent on the NSX-KACHINA node itself (matched by node name): adds board model, temps, power control
  - {id: kachina-agent, name: "NSX-KACHINA agent", url: "http://100.64.0.5:9101", token_env: DEMO_TOKEN,
     node: NSX-KACHINA, power_control: true, wol_relay: true, control_token_env: DEMO_CONTROL_TOKEN}
  - {id: straw-pc, name: "Straw's Ryzen PC", url: "http://100.64.0.20:9101", token_env: DEMO_TOKEN,
     group: Straw, owner: straw, tags: [friend, gaming], power_links: {desk-plug: straw-pc}}
  - {id: asus-i7-agent, name: "ASUS-i7 agent", url: "http://100.64.0.22:9101", token_env: DEMO_TOKEN,
     group: Straw, owner: straw, links_to: straw-node-asus-i7,
     ups_powers: {cyberpower: [straw-node-asus-i7, straw-pc]}, ups_names: {cyberpower: "Straw's CyberPower"},
     wol_relay: true, control_token_env: DEMO_CONTROL_TOKEN}
  - {id: straw-laptop, name: "Straw's laptop", url: "http://100.64.0.21:9101", token_env: DEMO_TOKEN,
     group: Straw, owner: straw, tags: [friend, laptop]}
systems:
  - {id: qemu-100, dwservice_url: "https://www.dwservice.net/en/login.html?loginUser=demo-win11"}
  - {id: lxc-101, tags: [games]}
  - {id: straw-laptop, wol: {mac: "a8:5e:45:12:34:56", relay: asus-i7-agent, broadcast: 192.168.1.255}}
  - {id: nsx-cyrene, wol: {mac: "3c:ec:ef:00:11:22", relay: kachina-agent, broadcast: 192.168.10.255}}
panel_hosts: [node-nsx-kachina, lxc-110]
ups:
  - {id: rack, type: demo, name: "Rack UPS · APC Smart-UPS 1500", powers: [node-nsx-fontaine, node-nsx-kachina, node-nsx-snezhnaya]}
power_sources:
  - {id: plug-cyrene, type: demo, name: "Tuya plug · NSX-CYRENE", system: nsx-cyrene, base_w: 95, swing_w: 140, period_s: 120}
  - {id: plug-llm-box, type: demo, name: "Tuya plug · llm-box", system: llm-box, base_w: 190, swing_w: 665, profile: llm}
  - {id: ups-rack, type: demo, name: "Rack UPS (NUT)", base_w: 210, swing_w: 60, period_s: 200}
`

// Config builds the demo configuration (data under dir).
func Config(dir, publicURL string) (*config.Panel, error) {
	for k, v := range map[string]string{"DEMO_TOKEN": randTok(), "DEMO_CONTROL_TOKEN": randTok(),
		"DEMO_PVE_TOKEN": "demo-not-a-real-token", "DEMO_STRAW_PVE_TOKEN": "demo-not-a-real-token",
		"DEMO_PVE_POWER_TOKEN": "demo-not-a-real-token"} {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return config.Parse([]byte(fmt.Sprintf(configYAML, publicURL, filepath.Join(dir, "demo-audit.log"), filepath.Join(dir, "data"))))
}
