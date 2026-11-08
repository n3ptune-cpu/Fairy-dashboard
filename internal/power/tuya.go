package power

import (
	"context"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/tuya"
)

type TuyaConfig struct {
	Base        `yaml:",inline"`
	DeviceID    string                      `yaml:"device_id"`
	IP          string                      `yaml:"ip"`
	LocalKeyEnv string                      `yaml:"local_key_env"`
	Version     string                      `yaml:"version"`
	DPS         map[string]devices.DpsField `yaml:"dps"`
	RefreshDPS  *[]int                      `yaml:"refresh_dps"` // some plugs only update power DPS on request
}

// tuyaSource: Tuya devices accept ONE TCP connection and can drop/reset under aggressive polling, so this keeps a
// single persistent connection and defaults to 5 s (floor 3 s).
type tuyaSource struct {
	defaults
	c   TuyaConfig
	mu  sync.Mutex
	dev *tuya.Device
}

func (s *tuyaSource) Info() Base { return s.c.Base }

func (s *tuyaSource) DefaultInterval() float64 { return 5 }

func (s *tuyaSource) FloorInterval() float64 { return 3 }

func (s *tuyaSource) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dev != nil {
		s.dev.Close()
		s.dev = nil
	}
}

func (s *tuyaSource) Read(_ context.Context, timeout time.Duration) ([]model.PowerReading, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.c
	if s.dev == nil {
		key, err := config.EnvSecret(c.LocalKeyEnv, "power source "+c.ID, 16)
		if err != nil {
			return nil, devices.Errf("%s", err.Error())
		}
		d, err := tuya.NewDevice(c.DeviceID, c.IP, key, c.Version, timeout)
		if err != nil {
			return nil, devices.Errf("%s", err.Error())
		}
		s.dev = d
	}
	s.dev.Timeout = timeout
	refresh := []int{18, 19, 20}
	if c.RefreshDPS != nil {
		refresh = *c.RefreshDPS
	}
	dps, err := s.dev.Status(refresh)
	if err != nil {
		return nil, devices.Errf("%s", err.Error()) // tuya errors never contain the key
	}
	return []model.PowerReading{Reading(c.Base, devices.ParseTuyaDPS(dps, c.DPS))}, nil
}
