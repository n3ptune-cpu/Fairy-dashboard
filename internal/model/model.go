// Package model holds the JSON API types. Field names and shapes match the Python prototype exactly, so the
// embedded frontend (and any scripts using the API) work unchanged. Nullable fields are pointers (JSON null);
// lists are never null (see Normalize).
package model

import (
	"reflect"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/power"
)

// P returns a pointer to v (for optional fields).
func P[T any](v T) *T { return &v }

// Deref returns *p, or the zero value ("" for strings) when p is nil.
func Deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// Temperature is one temperature sensor.
type Temperature struct {
	Sensor    string   `json:"sensor"`
	Label     *string  `json:"label"`
	CurrentC  float64  `json:"current_c"`
	HighC     *float64 `json:"high_c"`
	CriticalC *float64 `json:"critical_c"`
}

// Fan is one fan's speed.
type Fan struct {
	Name string  `json:"name"`
	RPM  float64 `json:"rpm"`
}

// Voltage is one voltage sensor.
type Voltage struct {
	Name  string  `json:"name"`
	Volts float64 `json:"volts"`
}

// Disk is one mounted filesystem.
type Disk struct {
	Mount  string  `json:"mount"`
	FSType *string `json:"fstype"`
	Used   int64   `json:"used"`
	Total  int64   `json:"total"`
}

// Gpu is one graphics card.
type Gpu struct {
	Name        string   `json:"name"`
	Vendor      string   `json:"vendor"` // nvidia | amd | intel | other
	UtilPercent *float64 `json:"util_percent"`
	VRAMUsed    *int64   `json:"vram_used"`
	VRAMTotal   *int64   `json:"vram_total"`
	TempC       *float64 `json:"temp_c"`
	PowerW      *float64 `json:"power_w"`
	Source      string   `json:"source,omitempty"` // "windows": Windows' own GPU counters (no temperature or watts); "amd": plus AMD's ADL
}

// PowerSensor is one named power reading from an agent.
type PowerSensor struct {
	Name  string  `json:"name"`
	Watts float64 `json:"watts"`
}

// ComponentPower is what an agent measures inside the host (CPU package, GPU, battery, BMC).
type ComponentPower struct {
	CPUPackageW  *float64      `json:"cpu_package_w"`
	GPUW         *float64      `json:"gpu_w"`
	BatteryW     *float64      `json:"battery_w"`
	BatteryState *string       `json:"battery_state"`
	BMCW         *float64      `json:"bmc_w"`
	BMCNote      *string       `json:"bmc_note"`
	Sensors      []PowerSensor `json:"sensors"`
	Source       []string      `json:"source"`
}

// Battery is a laptop's battery as its agent reports it (nil for machines without one). PowerW is the battery's
// draw while discharging (then effectively the whole machine's draw), negative while charging.
type Battery struct {
	Percent        *float64 `json:"percent"`
	State          string   `json:"state"` // charging | discharging | full | idle | unknown
	ACOnline       *bool    `json:"ac_online"`
	PowerW         *float64 `json:"power_w"`
	TimeRemainingS *int64   `json:"time_remaining_s"`
	HealthPct      *float64 `json:"health_pct"`
}

// Hardware is the host's DMI / board information; Display is the readable "vendor model".
type Hardware struct {
	SystemVendor *string `json:"system_vendor"`
	SystemModel  *string `json:"system_model"`
	BoardVendor  *string `json:"board_vendor"`
	BoardModel   *string `json:"board_model"`
	BIOSVersion  *string `json:"bios_version"`
	Display      *string `json:"display"`
}

// OSIcon is the mark the page draws for a host's operating system (normalise.OSIcon): Key names one of the marks
// in static/osicons.js ("windows11", "windows10", "windows", "proxmox", "ubuntu", "debian", ..., "linux" for any
// other distribution, "macos"), Name is the system's short name ("Windows 11", "Ubuntu", "Proxmox VE").
type OSIcon struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// Helper is the Windows agent's elevated helper (Fairy-agent v0.5.0+): a separate LocalSystem service,
// "fairy-agent-helper", with no network access, that reads Core Temp and the BMC's sensors, drives the fans (with
// the agent's temperature watchdog), runs power actions and installs updates, while the agent itself runs as an
// unprivileged account. Linux agents and older agents send no helper block (System.Helper is nil).
type Helper struct {
	State    string     `json:"state"`   // running | stopped | starting | not_installed | error | unknown
	Needed   bool       `json:"needed"`  // this host uses something only the helper can do
	Version  string     `json:"version"` // of the running helper ("" while it does not run)
	Since    *time.Time `json:"since"`   // in the current state since
	Watchdog bool       `json:"watchdog"`
	Features []string   `json:"features"` // what it does on this host: coretemp | ipmi | fans | power (| update)
	Error    string     `json:"error"`
	// Paused is the helper's readings this host normally shows that are missing because the helper is not
	// running: coretemp (Core Temp), ipmi (BMC sensors), fans (fan control). Empty while it runs; only groups this
	// host actually had while the helper ran (or that the agent marks as helper_stopped) are listed.
	Paused []string `json:"paused"`
}

// PowerPending is a scheduled (cancellable) shutdown or reboot.
type PowerPending struct {
	Action      string    `json:"action"` // shutdown | reboot
	At          time.Time `json:"at"`
	DelayS      int       `json:"delay_s"`
	RequestedBy *string   `json:"requested_by"`
}

// System is one host, node or guest as the API and UI show it.
type System struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	Kind               string          `json:"kind"`   // node | qemu | lxc | agent
	Source             string          `json:"source"` // proxmox | agent
	Parent             *string         `json:"parent"`
	ClusterID          *string         `json:"cluster_id"`
	Cluster            *string         `json:"cluster"`
	VMID               *int            `json:"vmid"`
	Status             string          `json:"status"` // online | offline | stopped | stale | paused
	CPUPercent         *float64        `json:"cpu_percent"`
	Cores              *int            `json:"cores"`
	MemUsed            *int64          `json:"mem_used"`
	MemTotal           *int64          `json:"mem_total"`
	DiskUsed           *int64          `json:"disk_used"`
	DiskTotal          *int64          `json:"disk_total"`
	Disks              []Disk          `json:"disks"`
	NetRxBps           *float64        `json:"net_rx_bps"`
	NetTxBps           *float64        `json:"net_tx_bps"`
	UptimeS            *int64          `json:"uptime_s"`
	OS                 *string         `json:"os"`
	Platform           *string         `json:"platform"`
	OSIcon             *OSIcon         `json:"os_icon"`  // from kind, platform and os; null: not known
	LoadAvg            []float64       `json:"load_avg"` // may be null
	Temps              []Temperature   `json:"temps"`
	Fans               []Fan           `json:"fans"`
	Voltages           []Voltage       `json:"voltages"`
	GPUs               []Gpu           `json:"gpus"`
	PowerW             *float64        `json:"power_w"`
	ComponentPower     *ComponentPower `json:"component_power"`
	Battery            *Battery        `json:"battery"`
	UPS                []string        `json:"ups"`
	FanControl         bool            `json:"fan_control"`
	ProcessesAvailable bool            `json:"processes_available"`
	Group              *string         `json:"group"`
	Owner              *string         `json:"owner"`
	Tags               []string        `json:"tags"`
	DWServiceURL       *string         `json:"dwservice_url"`
	LastSeen           *time.Time      `json:"last_seen"`
	Error              *string         `json:"error"`
	UpdatedAt          *time.Time      `json:"updated_at"`
	Hardware           *Hardware       `json:"hardware"`
	// CustomModel is the model name an admin set for this host in the panel (null: none). The page shows it
	// in place of Hardware.Display, which stays what the machine reports.
	CustomModel  *string       `json:"custom_model"`
	CPUModel     *string       `json:"cpu_model"`
	PowerPending *PowerPending `json:"power_pending"`
	// Sleep is what the host's agent says about sleep (nil: the agent does not say, older than v0.4.2):
	// true, or false with SleepNote saying why. Not copied onto a Proxmox node an agent runs on.
	Sleep     *bool   `json:"sleep,omitempty"`
	SleepNote *string `json:"sleep_note,omitempty"`
	// Helper is the Windows agent's elevated helper (nil: Linux, or an agent older than v0.5.0).
	Helper *Helper `json:"helper,omitempty"`
	// PowerHelperReason: the agent's power actions are unavailable because its helper is not running
	// ("helper_stopped" / "helper_error"; power_control.available false). Internal: decides the power menu.
	PowerHelperReason string `json:"-"`
	// Asleep: offline because the panel sent it to sleep (until it is seen online again).
	Asleep bool `json:"asleep,omitempty"`
	// BMCRelayTargets: the bmc_relay target ids this agent reports (nil: no relay, or older than v0.4.2).
	// Internal: decides whether "Power on (via BMC)" is offered for the machines it can wake.
	BMCRelayTargets []string `json:"-"`
	PowerActions    []string `json:"power_actions"`
	PollIntervalS   *float64 `json:"poll_interval_s"`
	PollMode        string   `json:"poll_mode"` // default | override | paused
	Boosted         bool     `json:"boosted"`
}

// SystemList is GET /api/systems.
type SystemList struct {
	GeneratedAt time.Time `json:"generated_at"`
	Systems     []System  `json:"systems"`
}

// Process is one row of an agent's process list.
type Process struct {
	PID         int      `json:"pid"`
	Name        *string  `json:"name"`
	Username    *string  `json:"username"`
	Status      *string  `json:"status"`
	CPUPercent  *float64 `json:"cpu_percent"`
	RSS         *int64   `json:"rss"`
	MemPercent  *float64 `json:"mem_percent"`
	GPUMem      *int64   `json:"gpu_mem"`
	GPUMemByGPU []int64  `json:"gpu_mem_by_gpu"` // may be null
	CreateTime  *float64 `json:"create_time"`
	Cmdline     *string  `json:"cmdline"`
}

// ProcessList is GET /api/systems/{id}/processes.
type ProcessList struct {
	SystemID       string    `json:"system_id"`
	DisabledByHost bool      `json:"disabled_by_host"`
	SampledAt      *float64  `json:"sampled_at"`
	Sort           string    `json:"sort"`
	TotalProcesses *int      `json:"total_processes"`
	Processes      []Process `json:"processes"`
}

// PowerReading is one measurement of a power source (defined with the sources in fairy-agent's pkg/power).
type PowerReading = power.Reading

// PowerSourceState is one power source (configured or relayed by an agent) with its latest readings.
type PowerSourceState struct {
	ID             string         `json:"id"`
	Type           string         `json:"type"`
	Name           string         `json:"name"`
	Status         string         `json:"status"` // ok | stale | unreachable
	IncludeInTotal bool           `json:"include_in_total"`
	Readings       []PowerReading `json:"readings"`
	LastSuccess    *time.Time     `json:"last_success"`
	Error          *string        `json:"error"`
	NextAttemptInS *float64       `json:"next_attempt_in_s"`
	Via            *string        `json:"via"`
}

// PowerOverview is GET /api/power.
type PowerOverview struct {
	GeneratedAt time.Time          `json:"generated_at"`
	TotalW      *float64           `json:"total_w"`
	Sources     []PowerSourceState `json:"sources"`
}

// PowerPoint is one history sample: the total and each source's watts.
type PowerPoint struct {
	T       time.Time           `json:"t"`
	TotalW  *float64            `json:"total_w"`
	Sources map[string]*float64 `json:"sources"`
}

// PowerHistory is GET /api/power/history.
type PowerHistory struct {
	IntervalS float64      `json:"interval_s"`
	Points    []PowerPoint `json:"points"`
}

// SourceStatus is the health of one polled source (a Proxmox cluster or an agent).
type SourceStatus struct {
	Name        string     `json:"name"`
	OK          bool       `json:"ok"`
	LastSuccess *time.Time `json:"last_success"`
	LastError   *string    `json:"last_error"`
	LatencyMS   *float64   `json:"latency_ms"`
}

// ClusterTotals sums up one Proxmox cluster (or standalone host).
type ClusterTotals struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Standalone    bool     `json:"standalone"`
	Group         *string  `json:"group"`
	Owner         *string  `json:"owner"`
	NodesOnline   int      `json:"nodes_online"`
	NodesTotal    int      `json:"nodes_total"`
	Cores         int      `json:"cores"`
	CPUPercent    *float64 `json:"cpu_percent"`
	MemUsed       int64    `json:"mem_used"`
	MemTotal      int64    `json:"mem_total"`
	GuestsRunning int      `json:"guests_running"`
	GuestsTotal   int      `json:"guests_total"`
}

// HostTotals adds up every host a viewer may see: all Proxmox nodes of all clusters plus the agent hosts that
// are not merged into a node (nor run inside a guest). Only online hosts count towards cores, CPU and memory.
type HostTotals struct {
	Hosts       int      `json:"hosts"`
	HostsOnline int      `json:"hosts_online"`
	Cores       int      `json:"cores"`
	CPUPercent  *float64 `json:"cpu_percent"`
	MemUsed     int64    `json:"mem_used"`
	MemTotal    int64    `json:"mem_total"`
}

// Counts are the systems per status.
type Counts struct {
	Total   int `json:"total"`
	Online  int `json:"online"`
	Offline int `json:"offline"`
	Stopped int `json:"stopped"`
	Stale   int `json:"stale"`
	Paused  int `json:"paused"`
}

// ControlState says who turned control off, when and why (the kill switch).
type ControlState struct {
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// Summary is GET /api/summary.
type Summary struct {
	ControlDisabled *ControlState   `json:"control_disabled"`
	GeneratedAt     time.Time       `json:"generated_at"`
	Cluster         *ClusterTotals  `json:"cluster"`
	Clusters        []ClusterTotals `json:"clusters"`
	Totals          HostTotals      `json:"totals"`
	Counts          Counts          `json:"counts"`
	ByKind          map[string]int  `json:"by_kind"`
	Sources         []SourceStatus  `json:"sources"`
}

// User is the signed-in user (session), the API token's identity, or the demo user.
type User struct {
	Sub               string   `json:"sub"`
	Name              *string  `json:"name"`
	PreferredUsername *string  `json:"preferred_username"`
	Email             *string  `json:"email"`
	EmailVerified     bool     `json:"email_verified,omitempty"` // the IdP's email_verified claim was true
	Picture           *string  `json:"picture"`
	Groups            []string `json:"groups"`
	Auth              string   `json:"auth"` // session | token | demo
	TokenName         *string  `json:"token_name"`
	TokenScope        *string  `json:"token_scope"`
	TokenOwner        *string  `json:"token_owner"`
}

// Me is GET /api/me.
type Me struct {
	User
	CanControl      bool          `json:"can_control"`
	Demo            bool          `json:"demo"`
	ControlDisabled *ControlState `json:"control_disabled"`
	StepUpWindowS   int           `json:"step_up_window_s"`
	AuthAgeS        *float64      `json:"auth_age_s"`
	// Role is admin, viewer or pending (signed in, waiting for an admin to give access). Sites are the sites a
	// viewer sees; AllSites is true for admins (and read tokens without a site list).
	Role           string   `json:"role"`
	Sites          []string `json:"sites"`
	AllSites       bool     `json:"all_sites"`
	CanManageUsers bool     `json:"can_manage_users"`
	// Demo mode only: the role being previewed ("admin", "viewer:Bob", ...) and the choices.
	DemoView  string   `json:"demo_view,omitempty"`
	DemoViews []string `json:"demo_views,omitempty"`
	// PendingResult is the outcome of an action the panel ran right after a step-up sign-in (shown once).
	PendingResult *PendingResult `json:"pending_result,omitempty"`
	// Docs: the panel serves /docs (enable_docs: true). The page shows its "API docs" link only then.
	Docs bool `json:"docs"`
}

// PendingResult is what happened to the action that waited for a step-up sign-in.
type PendingResult struct {
	Label   string `json:"label"`
	OK      bool   `json:"ok"`
	Status  int    `json:"status"`
	Message string `json:"message"`
	Path    string `json:"path"`
	Ref     string `json:"ref,omitempty"` // the id of what the action made (an Add host), if any
}

// FanEvent is one entry of the agent's fan log (mode changes, watchdog).
type FanEvent struct {
	T      float64 `json:"t"`
	Action string  `json:"action"`
	Detail string  `json:"detail"`
}

// FanStatus is GET /api/systems/{id}/fan.
type FanStatus struct {
	SystemID    string         `json:"system_id"`
	Enabled     bool           `json:"enabled"`
	Vendor      *string        `json:"vendor"`
	Mode        *string        `json:"mode"`
	Error       *string        `json:"error"`
	Overridden  bool           `json:"overridden"`
	Duty        map[string]int `json:"duty"`
	MinDuty     *int           `json:"min_duty"`
	MaxTempC    *float64       `json:"max_temp_c"`
	AutoMode    *string        `json:"auto_mode"`         // the agent's fan_control.auto_mode (nil: older agent)
	Unsupported []string       `json:"unsupported_modes"` // modes this board's BMC refused (empty: none, or older agent)
	// Available is false while the agent cannot reach the fans (Fairy-agent v0.5.0 on Windows: its elevated
	// helper is not running; Reason says why). nil: older agents, which do not say.
	Available *bool      `json:"available,omitempty"`
	Reason    string     `json:"reason,omitempty"` // helper_stopped | helper_error
	Fans      []Fan      `json:"fans"`
	Events    []FanEvent `json:"events"`
}

// ControlResult is the answer to a fan write: the agent's reply passed through.
type ControlResult struct {
	OK     bool           `json:"ok"`
	Result map[string]any `json:"result"`
}

// UpsState is one UPS.
type UpsState struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	UpsName         string     `json:"ups_name"`
	Via             *string    `json:"via"`
	Status          string     `json:"status"`
	Stale           bool       `json:"stale"`
	Flags           []string   `json:"flags"`
	StatusRaw       *string    `json:"status_raw"`
	BatteryCharge   *float64   `json:"battery_charge"`
	BatteryRuntimeS *int       `json:"battery_runtime_s"`
	BatteryVoltage  *float64   `json:"battery_voltage"`
	LoadPercent     *float64   `json:"load_percent"`
	Watts           *float64   `json:"watts"`
	WattsEstimated  bool       `json:"watts_estimated"`
	NominalW        *float64   `json:"nominal_w"`
	InputVoltage    *float64   `json:"input_voltage"`
	OutputVoltage   *float64   `json:"output_voltage"`
	InputFrequency  *float64   `json:"input_frequency"`
	Model           *string    `json:"model"`
	Mfr             *string    `json:"mfr"`
	Serial          *string    `json:"serial"`
	BatteryDate     *string    `json:"battery_date"`
	TestResult      *string    `json:"test_result"`
	TransferReason  *string    `json:"transfer_reason"`
	OnBatterySince  *time.Time `json:"on_battery_since"`
	Powers          []string   `json:"powers"`
	LastSeen        *time.Time `json:"last_seen"`
	Error           *string    `json:"error"`
}

// UpsEvent is a UPS transition (on battery, power restored, low battery, ...).
type UpsEvent struct {
	Seq       int       `json:"seq"`
	T         time.Time `json:"t"`
	UpsID     string    `json:"ups_id"`
	UpsName   string    `json:"ups_name"`
	Kind      string    `json:"kind"`
	Severity  string    `json:"severity"`
	Message   string    `json:"message"`
	StatusRaw *string   `json:"status_raw"`
}

// UpsPoint is one UPS history sample.
type UpsPoint struct {
	T        time.Time `json:"t"`
	Charge   *float64  `json:"charge"`
	Load     *float64  `json:"load"`
	Watts    *float64  `json:"watts"`
	RuntimeS *int      `json:"runtime_s"`
}

// UpsOverview is GET /api/ups.
type UpsOverview struct {
	GeneratedAt time.Time             `json:"generated_at"`
	UPS         []UpsState            `json:"ups"`
	History     map[string][]UpsPoint `json:"history"`
}

// PollJob describes one polling job that feeds a system.
type PollJob struct {
	Job             string   `json:"job"`
	DefaultS        float64  `json:"default_s"`
	FloorS          float64  `json:"floor_s"`
	EffectiveS      *float64 `json:"effective_s"`
	Clamped         bool     `json:"clamped"`
	Boosted         bool     `json:"boosted"`
	Paused          bool     `json:"paused"`
	Shared          bool     `json:"shared"`
	Runs            int      `json:"runs"`
	SkippedOverlaps int      `json:"skipped_overlaps"`
	LastDurationMS  *float64 `json:"last_duration_ms"`
}

// PollStatus is GET /api/systems/{id}/poll.
type PollStatus struct {
	SystemID     string    `json:"system_id"`
	Mode         string    `json:"mode"`
	RequestedS   *float64  `json:"requested_s"`
	EffectiveS   *float64  `json:"effective_s"`
	Boosted      bool      `json:"boosted"`
	BoostEnabled bool      `json:"boost_enabled"`
	Jobs         []PollJob `json:"jobs"`
	Note         *string   `json:"note"`
}

// PowerActionCap is one power action of a system: whether it is available (or why not) and how to confirm it.
type PowerActionCap struct {
	Action    string  `json:"action"`
	Available bool    `json:"available"`
	Via       *string `json:"via"`
	ViaLabel  *string `json:"via_label"`
	Reason    *string `json:"reason"`
	Confirm   string  `json:"confirm"` // none | click | typed
	Delayed   bool    `json:"delayed"`
}

// PowerCaps is GET /api/systems/{id}/power.
type PowerCaps struct {
	SystemID   string           `json:"system_id"`
	Name       string           `json:"name"`
	Actions    []PowerActionCap `json:"actions"`
	Pending    *PowerPending    `json:"pending"`
	KillsPanel bool             `json:"kills_panel"`
	Warning    *string          `json:"warning"`
}

// PowerResult is the answer to POST /api/systems/{id}/power.
type PowerResult struct {
	SystemID string        `json:"system_id"`
	Action   string        `json:"action"`
	Via      string        `json:"via"`
	Pending  *PowerPending `json:"pending"`
	Message  string        `json:"message"`
}

// AuditPage is one page of GET /api/audit, newest entry first.
//
// Entries are the audit-log lines as written (see audit.Entry) plus "id", the entry's byte offset. They stay
// generic maps so that lines written by older versions, with other keys, are passed through unchanged.
type AuditPage struct {
	Entries []map[string]any `json:"entries"`
	Next    *int64           `json:"next"` // pass as ?before= for the next page; null on the last page
}

// Normalize replaces nil slices and maps with empty ones (recursively), so JSON never has `null` where the
// frontend expects a list. Fields that are deliberately nullable lists (load_avg, gpu_mem_by_gpu) are kept.
func Normalize(v any) { normalize(reflect.ValueOf(v)) }

var keepNil = map[string]bool{"LoadAvg": true, "GPUMemByGPU": true}

func normalize(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			normalize(v.Elem())
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if !f.CanSet() {
				continue
			}
			if keepNil[t.Field(i).Name] {
				continue
			}
			switch f.Kind() {
			case reflect.Slice:
				if f.IsNil() {
					f.Set(reflect.MakeSlice(f.Type(), 0, 0))
				}
			case reflect.Map:
				if f.IsNil() {
					f.Set(reflect.MakeMap(f.Type()))
				}
			}
			normalize(f)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			normalize(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			e := v.MapIndex(k)
			if e.Kind() == reflect.Pointer || e.Kind() == reflect.Interface {
				normalize(e)
			}
		}
	}
}
