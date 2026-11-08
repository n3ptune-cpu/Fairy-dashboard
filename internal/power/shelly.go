package power

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

type ShellyConfig struct {
	Base        `yaml:",inline"`
	URL         string `yaml:"url"` // http://192.168.1.50
	Gen         string `yaml:"gen"` // auto | 1 | 2
	Channel     int    `yaml:"channel"`
	RPCMethod   string `yaml:"rpc_method"` // e.g. PM1.GetStatus for Shelly PM Mini
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
}

type shelly struct {
	defaults
	c ShellyConfig
}

func (s *shelly) Info() Base { return s.c.Base }

func (s *shelly) Read(ctx context.Context, timeout time.Duration) ([]model.PowerReading, error) {
	c := s.c
	pw, err := secret(c.PasswordEnv, "power source "+c.ID)
	if err != nil {
		return nil, devices.Errf("%s", err.Error())
	}
	base := strings.TrimRight(c.URL, "/")
	hc := devices.LANClient(timeout)
	if c.Gen == "auto" || c.Gen == "2" {
		auth := ""
		if pw != "" {
			auth = "digest" // Gen2 auth is digest
		}
		d, err := devices.GetJSON(ctx, hc, fmt.Sprintf("%s/rpc/%s?id=%d", base, url.PathEscape(c.RPCMethod), c.Channel), auth, c.Username, pw)
		var he *devices.HTTPStatusError
		if err == nil {
			return []model.PowerReading{Reading(c.Base, devices.ParseShellyGen2(d))}, nil
		}
		if !errors.As(err, &he) || he.Code != 404 || c.Gen == "2" {
			return nil, err
		}
	}
	auth := ""
	if pw != "" {
		auth = "basic"
	}
	d, err := devices.GetJSON(ctx, hc, base+"/status", auth, c.Username, pw)
	if err != nil {
		return nil, err
	}
	r, err := devices.ParseShellyGen1(d, c.Channel)
	if err != nil {
		return nil, err
	}
	return []model.PowerReading{Reading(c.Base, r)}, nil
}
