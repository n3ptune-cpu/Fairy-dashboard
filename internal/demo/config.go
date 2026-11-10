package demo

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

const configYAML = `
public_url: %q
offline_after: 30
enable_docs: true
audit_log: %q
data_dir: %q
oidc: {issuer: "https://id.example.invalid", client_id: demo, control_groups: [controllers]}
proxmox:
  - {id: dc1, id_prefix: "", cluster_name: SRV-DC1, site: Alice, urls: ["https://srv-pvea.invalid:8006"],
     token_id: "fairy@pve!panel", token_secret_env: DEMO_PVE_TOKEN,
     # separate token with only the FairyPower role (VM.PowerMgmt) -> guest start/shutdown/reboot/stop
     power_token_id: "fairy@pve!power", power_token_secret_env: DEMO_PVE_POWER_TOKEN}
  # Bob's standalone PVE host, reached over Tailscale node sharing with a read-only token he gave Alice
  - {id: bob, cluster_name: MINI-i7, standalone: true, group: Bob, owner: bob, site: Bob,
     urls: ["https://mini-i7.bobs-tailnet.invalid:8006"], token_id: "fairy@pve!panel", token_secret_env: DEMO_BOB_PVE_TOKEN}
agents:
  - {id: srv-tower, name: SRV-TOWER, url: "http://100.64.0.10:9101", token_env: DEMO_TOKEN, group: Alice, owner: alice, site: Alice,
     tags: [server, supermicro], dwservice_url: "https://www.dwservice.net/en/login.html?loginUser=demo-tower",
     fan_control: {vendor: supermicro, control_token_env: DEMO_CONTROL_TOKEN}, power_control: true, self_update: true}
  - {id: alice-laptop, name: "ASUS Vivobook 16X", url: "http://100.64.0.11:9101", token_env: DEMO_TOKEN,
     group: Alice, owner: alice, site: Alice, tags: [laptop], power_control: true, control_token_env: DEMO_CONTROL_TOKEN, self_update: true,
     dwservice_url: "https://www.dwservice.net/en/login.html?loginUser=demo-laptop"}
  - {id: llm-box, name: llm-box, url: "http://100.64.0.12:9101", token_env: DEMO_TOKEN, group: Alice, owner: alice, site: Alice, tags: [llm]}
  # agent on the SRV-PVEA node itself (matched by node name): adds board model, temps, power control
  - {id: pvea-agent, name: "SRV-PVEA agent", url: "http://100.64.0.5:9101", token_env: DEMO_TOKEN,
     node: SRV-PVEA, power_control: true, wol_relay: true, control_token_env: DEMO_CONTROL_TOKEN, self_update: true}
  - {id: bob-pc, name: "Bob's Ryzen PC", url: "http://100.64.0.20:9101", token_env: DEMO_TOKEN,
     group: Bob, owner: bob, site: Bob, tags: [friend, gaming], power_links: {desk-plug: bob-pc}}
  - {id: mini-i7-agent, name: "MINI-i7 agent", url: "http://100.64.0.22:9101", token_env: DEMO_TOKEN,
     group: Bob, owner: bob, site: Bob, links_to: bob-node-mini-i7,
     ups_powers: {cyberpower: [bob-node-mini-i7, bob-pc]}, ups_names: {cyberpower: "Bob's CyberPower"},
     wol_relay: true, control_token_env: DEMO_CONTROL_TOKEN}
  - {id: bob-laptop, name: "Bob's laptop", url: "http://100.64.0.21:9101", token_env: DEMO_TOKEN,
     group: Bob, owner: bob, site: Bob, tags: [friend, laptop]}
systems:
  - {id: qemu-100, dwservice_url: "https://www.dwservice.net/en/login.html?loginUser=demo-win11"}
  - {id: lxc-101, tags: [games]}
  - {id: bob-laptop, wol: {mac: "00:00:5e:00:53:04", relay: mini-i7-agent, broadcast: 192.168.1.255}}
  # the BMC shares the LAN port (no Wake-on-LAN when off): the laptop, cabled to it, powers it on through the BMC
  - {id: srv-tower, wol: {mac: "00:00:5e:00:53:01", relay: pvea-agent, broadcast: 192.168.10.255},
     bmc_power: {relay: alice-laptop, target: tower-bmc}}
panel_hosts: [node-srv-pvea, lxc-110]
ups:
  - {id: rack, type: demo, name: "Rack UPS · APC Smart-UPS 1500", powers: [node-srv-pvec, node-srv-pvea, node-srv-pveb]}
power_sources:
  - {id: plug-tower, type: demo, name: "Tuya plug · SRV-TOWER", system: srv-tower, base_w: 95, swing_w: 140, period_s: 120}
  - {id: plug-llm-box, type: demo, name: "Tuya plug · llm-box", system: llm-box, base_w: 190, swing_w: 665, profile: llm}
  - {id: ups-rack, type: demo, name: "Rack UPS (NUT)", base_w: 210, swing_w: 60, period_s: 200}
`

// Config builds the demo configuration (data under dir).
func Config(dir, publicURL string) (*config.Panel, error) {
	for k, v := range map[string]string{"DEMO_TOKEN": randTok(), "DEMO_CONTROL_TOKEN": randTok(),
		"DEMO_PVE_TOKEN": "demo-not-a-real-token", "DEMO_BOB_PVE_TOKEN": "demo-not-a-real-token",
		"DEMO_PVE_POWER_TOKEN": "demo-not-a-real-token"} {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return config.Parse([]byte(fmt.Sprintf(configYAML, publicURL, filepath.Join(dir, "demo-audit.log"), filepath.Join(dir, "data"))))
}
