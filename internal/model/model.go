// Package model holds the JSON API types. Field names and shapes match the Python prototype exactly, so the
// embedded frontend (and any scripts using the API) work unchanged. Nullable fields are pointers (JSON null);
// lists are never null (see Normalize).
package model

import (
	"reflect"
	"time"
)

// P returns a pointer to v (for optional fields).
func P[T any](v T) *T { return &v }

type Temperature struct {
	Sensor    string   `json:"sensor"`
	Label     *string  `json:"label"`
	CurrentC  float64  `json:"current_c"`
	HighC     *float64 `json:"high_c"`
	CriticalC *float64 `json:"critical_c"`
}

type Fan struct {
	Name string  `json:"name"`
	RPM  float64 `json:"rpm"`
}

type Voltage struct {
	Name  string  `json:"name"`
	Volts float64 `json:"volts"`
}

type Disk struct {
	Mount  string  `json:"mount"`
	FSType *string `json:"fstype"`
	Used   int64   `json:"used"`
	Total  int64   `json:"total"`
}

type Gpu struct {
	Name        string   `json:"name"`
	Vendor      string   `json:"vendor"` // nvidia | amd | intel | other
	UtilPercent *float64 `json:"util_percent"`
	VRAMUsed    *int64   `json:"vram_used"`
	VRAMTotal   *int64   `json:"vram_total"`
	TempC       *float64 `json:"temp_c"`
	PowerW      *float64 `json:"power_w"`
}

type PowerSensor struct {
	Name  string  `json:"name"`
	Watts float64 `json:"watts"`
}

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

type Hardware struct {
	SystemVendor *string `json:"system_vendor"`
	SystemModel  *string `json:"system_model"`
	BoardVendor  *string `json:"board_vendor"`
	BoardModel   *string `json:"board_model"`
	BIOSVersion  *string `json:"bios_version"`
	Display      *string `json:"display"`
}

type PowerPending struct {
	Action      string    `json:"action"` // shutdown | reboot
	At          time.Time `json:"at"`
	DelayS      int       `json:"delay_s"`
	RequestedBy *string   `json:"requested_by"`
}

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
	LoadAvg            []float64       `json:"load_avg"` // may be null
	Temps              []Temperature   `json:"temps"`
	Fans               []Fan           `json:"fans"`
	Voltages           []Voltage       `json:"voltages"`
	GPUs               []Gpu           `json:"gpus"`
	PowerW             *float64        `json:"power_w"`
	ComponentPower     *ComponentPower `json:"component_power"`
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
	CPUModel           *string         `json:"cpu_model"`
	PowerPending       *PowerPending   `json:"power_pending"`
	PowerActions       []string        `json:"power_actions"`
	PollIntervalS      *float64        `json:"poll_interval_s"`
	PollMode           string          `json:"poll_mode"` // default | override | paused
	Boosted            bool            `json:"boosted"`
}

type SystemList struct {
	GeneratedAt time.Time `json:"generated_at"`
	Systems     []System  `json:"systems"`
}

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

type ProcessList struct {
	SystemID       string    `json:"system_id"`
	DisabledByHost bool      `json:"disabled_by_host"`
	SampledAt      *float64  `json:"sampled_at"`
	Sort           string    `json:"sort"`
	TotalProcesses *int      `json:"total_processes"`
	Processes      []Process `json:"processes"`
}

type PowerReading struct {
	SourceID       string    `json:"source_id"`
	Name           string    `json:"name"`
	Watts          *float64  `json:"watts"`
	Voltage        *float64  `json:"voltage"`
	Current        *float64  `json:"current"`
	EnergyKWhTotal *float64  `json:"energy_kwh_total"`
	SystemID       *string   `json:"system_id"`
	Timestamp      time.Time `json:"timestamp"`
}

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

type PowerOverview struct {
	GeneratedAt time.Time          `json:"generated_at"`
	TotalW      *float64           `json:"total_w"`
	Sources     []PowerSourceState `json:"sources"`
}

type PowerPoint struct {
	T       time.Time           `json:"t"`
	TotalW  *float64            `json:"total_w"`
	Sources map[string]*float64 `json:"sources"`
}

type PowerHistory struct {
	IntervalS float64      `json:"interval_s"`
	Points    []PowerPoint `json:"points"`
}

type SourceStatus struct {
	Name        string     `json:"name"`
	OK          bool       `json:"ok"`
	LastSuccess *time.Time `json:"last_success"`
	LastError   *string    `json:"last_error"`
	LatencyMS   *float64   `json:"latency_ms"`
}

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

type Counts struct {
	Total   int `json:"total"`
	Online  int `json:"online"`
	Offline int `json:"offline"`
	Stopped int `json:"stopped"`
	Stale   int `json:"stale"`
	Paused  int `json:"paused"`
}

type ControlState struct {
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

type Summary struct {
	ControlDisabled *ControlState   `json:"control_disabled"`
	GeneratedAt     time.Time       `json:"generated_at"`
	Cluster         *ClusterTotals  `json:"cluster"`
	Clusters        []ClusterTotals `json:"clusters"`
	Counts          Counts          `json:"counts"`
	ByKind          map[string]int  `json:"by_kind"`
	Sources         []SourceStatus  `json:"sources"`
}

type User struct {
	Sub               string   `json:"sub"`
	Name              *string  `json:"name"`
	PreferredUsername *string  `json:"preferred_username"`
	Email             *string  `json:"email"`
	Picture           *string  `json:"picture"`
	Groups            []string `json:"groups"`
	Auth              string   `json:"auth"` // session | token | demo
	TokenName         *string  `json:"token_name"`
	TokenScope        *string  `json:"token_scope"`
	TokenOwner        *string  `json:"token_owner"`
}

type Me struct {
	User
	CanControl      bool          `json:"can_control"`
	Demo            bool          `json:"demo"`
	ControlDisabled *ControlState `json:"control_disabled"`
	StepUpWindowS   int           `json:"step_up_window_s"`
	AuthAgeS        *float64      `json:"auth_age_s"`
}

type FanEvent struct {
	T      float64 `json:"t"`
	Action string  `json:"action"`
	Detail string  `json:"detail"`
}

type FanStatus struct {
	SystemID   string         `json:"system_id"`
	Enabled    bool           `json:"enabled"`
	Vendor     *string        `json:"vendor"`
	Mode       *string        `json:"mode"`
	Error      *string        `json:"error"`
	Overridden bool           `json:"overridden"`
	Duty       map[string]int `json:"duty"`
	MinDuty    *int           `json:"min_duty"`
	MaxTempC   *float64       `json:"max_temp_c"`
	Fans       []Fan          `json:"fans"`
	Events     []FanEvent     `json:"events"`
}

type ControlResult struct {
	OK     bool           `json:"ok"`
	Result map[string]any `json:"result"`
}

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

type UpsPoint struct {
	T        time.Time `json:"t"`
	Charge   *float64  `json:"charge"`
	Load     *float64  `json:"load"`
	Watts    *float64  `json:"watts"`
	RuntimeS *int      `json:"runtime_s"`
}

type UpsOverview struct {
	GeneratedAt time.Time             `json:"generated_at"`
	UPS         []UpsState            `json:"ups"`
	History     map[string][]UpsPoint `json:"history"`
}

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

type PowerActionCap struct {
	Action    string  `json:"action"`
	Available bool    `json:"available"`
	Via       *string `json:"via"`
	ViaLabel  *string `json:"via_label"`
	Reason    *string `json:"reason"`
	Confirm   string  `json:"confirm"` // none | click | typed
	Delayed   bool    `json:"delayed"`
}

type PowerCaps struct {
	SystemID   string           `json:"system_id"`
	Name       string           `json:"name"`
	Actions    []PowerActionCap `json:"actions"`
	Pending    *PowerPending    `json:"pending"`
	KillsPanel bool             `json:"kills_panel"`
	Warning    *string          `json:"warning"`
}

type PowerResult struct {
	SystemID string        `json:"system_id"`
	Action   string        `json:"action"`
	Via      string        `json:"via"`
	Pending  *PowerPending `json:"pending"`
	Message  string        `json:"message"`
}

type AuditEntry struct {
	ID      int64          `json:"id"`
	TS      string         `json:"ts"`
	Actor   string         `json:"actor"`
	Sub     *string        `json:"sub"`
	Action  string         `json:"action"`
	Target  string         `json:"target"`
	Params  map[string]any `json:"params"`
	Outcome string         `json:"outcome"`
	Source  string         `json:"source"`
	IP      *string        `json:"ip"`
}

type AuditPage struct {
	Entries []AuditEntry `json:"entries"`
	Next    *int64       `json:"next"`
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
