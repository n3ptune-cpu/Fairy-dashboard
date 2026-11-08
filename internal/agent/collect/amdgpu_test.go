package collect

import (
	"os"
	"runtime"
	"testing"
	"time"
)

func TestAMDGPUFixture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses symlinks")
	}
	sys := "testdata/amdgpu/sys"
	cards := AMDCards(sys)
	if len(cards) != 3 { // card0 is Intel (i915); card1-eDP-1 is a connector
		t.Fatalf("cards %v", cards)
	}
	a := NewAMDGPU(sys, time.Second)
	gpus := a.GPUs()
	if len(gpus) != 3 {
		t.Fatalf("%+v", gpus)
	}
	igpu, dgpu, asleep := gpus[0], gpus[1], gpus[2]
	// integrated: no product_name, power1_input, VRAM = the carve-out (GTT not added)
	if igpu.Name != "AMD Radeon (amdgpu 1002:15bf)" || igpu.Vendor != "amd" || *igpu.UtilPercent != 7 ||
		*igpu.VRAMUsed != 256<<20 || *igpu.VRAMTotal != 512<<20 || *igpu.TempC != 45 || *igpu.PowerW != 8.1 {
		t.Fatalf("igpu %+v", igpu)
	}
	if dgpu.Name != "AMD Radeon RX 6800 XT" || *dgpu.UtilPercent != 99 || *dgpu.VRAMTotal != 17163091968 ||
		*dgpu.TempC != 71 || *dgpu.PowerW != 250.1 {
		t.Fatalf("dgpu %+v", dgpu)
	}
	// runtime-suspended: not woken up, reported idle with nothing else
	if asleep.Name != "AMD Radeon (amdgpu 1002:7340)" || *asleep.UtilPercent != 0 || asleep.VRAMTotal != nil ||
		asleep.TempC != nil || asleep.PowerW != nil {
		t.Fatalf("asleep %+v", asleep)
	}
	if a.ComputeApps(t.Context()) != nil {
		t.Fatal("no per-process VRAM from sysfs")
	}
}

func TestAMDGPUNone(t *testing.T) {
	if NewAMDGPU(t.TempDir(), time.Second) != nil {
		t.Fatal("no cards = no reader")
	}
}

func TestReadAMDGPUBadValues(t *testing.T) {
	dir := t.TempDir()
	write := func(name, v string) {
		t.Helper()
		if err := writeFile(dir+"/"+name, v); err != nil {
			t.Fatal(err)
		}
	}
	write("gpu_busy_percent", "250")
	write("mem_info_vram_used", "900")
	write("mem_info_vram_total", "100") // used > total
	write("product_name", "bad\x01name")
	write("device", "zz")
	g := ReadAMDGPU(dir, 0)
	if g.UtilPercent != nil || g.VRAMUsed != nil || g.Name != "AMD Radeon (amdgpu)" {
		t.Fatalf("%+v", g)
	}
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s+"\n"), 0o644) }
