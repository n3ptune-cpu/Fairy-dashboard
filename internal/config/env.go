package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// LoadEnvFile reads KEY=VALUE lines (optional quotes, # comments) into the environment without overriding
// variables that are already set. A missing file is not an error.
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set && envNameRe.MatchString(k) {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// EnvSecret resolves a secret referenced by env-var name.
func EnvSecret(name, what string, minLen int) (string, error) {
	if name == "" {
		return "", nil
	}
	v := os.Getenv(name)
	if len(v) < max(minLen, 1) {
		return "", fmt.Errorf("%s: env var %s is missing or shorter than %d chars", what, name, max(minLen, 1))
	}
	return v, nil
}

// Secrets read from FAIRY_* environment variables.
type Secrets struct {
	SessionSecret      string
	OIDCClientSecret   string
	ProxmoxTokenSecret string
	InsecureCookies    bool
}

func LoadSecrets() (Secrets, error) {
	s := Secrets{
		SessionSecret:      os.Getenv("FAIRY_SESSION_SECRET"),
		OIDCClientSecret:   os.Getenv("FAIRY_OIDC_CLIENT_SECRET"),
		ProxmoxTokenSecret: os.Getenv("FAIRY_PROXMOX_TOKEN_SECRET"),
		InsecureCookies:    truthy(os.Getenv("FAIRY_INSECURE_COOKIES")),
	}
	if len(s.SessionSecret) < 32 {
		return s, errors.New("FAIRY_SESSION_SECRET is missing or shorter than 32 characters")
	}
	return s, nil
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
