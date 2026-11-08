package power

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

type RedfishConfig struct {
	Base        `yaml:",inline"`
	URL         string `yaml:"url"`        // https://idrac.lan
	ChassisID   string `yaml:"chassis_id"` // iDRAC default System.Embedded.1; HPE iLO uses "1"
	PowerPath   string `yaml:"power_path"` // override, e.g. /redfish/v1/Chassis/1/EnvironmentMetrics
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
	VerifyTLS   *bool  `yaml:"verify_tls"`
	CAFile      string `yaml:"ca_file"`
}

type redfish struct {
	c   RedfishConfig
	tls *tls.Config
}

func (s *redfish) Info() Base { return s.c.Base }

func (s *redfish) DefaultInterval() float64 { return 10 }

// BMC sensors update slowly; don't hammer them
func (s *redfish) FloorInterval() float64 { return 10 }

func (s *redfish) MaxBackoff() float64 { return 300 }

// BMCs are often not cabled: back off up to 5 min
func (s *redfish) Close() {}

func (s *redfish) Read(ctx context.Context, timeout time.Duration) ([]model.PowerReading, error) {
	c := s.c
	path := c.PowerPath
	if path == "" {
		path = "/redfish/v1/Chassis/" + url.PathEscape(c.ChassisID) + "/Power"
	}
	pw, err := secret(c.PasswordEnv, c.ID)
	if err != nil {
		return nil, devices.Errf("%s", err.Error())
	}
	d, err := devices.GetJSON(ctx, devices.TLSClient(timeout, s.tls), strings.TrimRight(c.URL, "/")+path, "basic", c.Username, pw)
	if err != nil {
		return nil, err
	}
	w, v, err := devices.ParseRedfishPower(d)
	if err != nil {
		return nil, err
	}
	return []model.PowerReading{Reading(c.Base, devices.Reading{Watts: w, Voltage: v})}, nil
}

// TLSConfig builds the client TLS settings (custom CA or verification off).
func TLSConfig(verify bool, caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if !verify {
		cfg.InsecureSkipVerify = true //nolint:gosec // owner's explicit choice for self-signed BMCs
		return cfg, nil
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("no certificates in " + caFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}
