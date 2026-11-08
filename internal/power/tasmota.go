package power

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

type TasmotaConfig struct {
	Base        `yaml:",inline"`
	URL         string `yaml:"url"`
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
}

type tasmota struct {
	defaults
	c TasmotaConfig
}

func (s *tasmota) Info() Base { return s.c.Base }

func (s *tasmota) Read(ctx context.Context, timeout time.Duration) ([]model.PowerReading, error) {
	c := s.c
	q := url.Values{"cmnd": {"Status 8"}}
	pw, err := secret(c.PasswordEnv, "power source "+c.ID)
	if err != nil {
		return nil, devices.Errf("%s", err.Error())
	}
	if pw != "" { // Tasmota's own scheme; the URL is never logged
		q.Set("user", c.Username)
		q.Set("password", pw)
	}
	d, err := devices.GetJSON(ctx, devices.LANClient(timeout), strings.TrimRight(c.URL, "/")+"/cm?"+q.Encode(), "", "", "")
	if err != nil {
		return nil, err
	}
	r, err := devices.ParseTasmotaStatus8(d)
	if err != nil {
		return nil, err
	}
	return []model.PowerReading{Reading(c.Base, r)}, nil
}
