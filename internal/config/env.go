package config

import (
	"bufio"
	"errors"
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

// Secrets read from FAIRY_* environment variables.
type Secrets struct {
	SessionSecret      string
	OIDCClientSecret   string
	ProxmoxTokenSecret string
	InsecureCookies    bool
}

// LoadSecrets reads the FAIRY_* secrets from the environment; FAIRY_SESSION_SECRET (32+ characters, not the
// example value: SessionSecretProblem) is required.
func LoadSecrets() (Secrets, error) {
	s := Secrets{
		SessionSecret:      os.Getenv("FAIRY_SESSION_SECRET"),
		OIDCClientSecret:   os.Getenv("FAIRY_OIDC_CLIENT_SECRET"),
		ProxmoxTokenSecret: os.Getenv("FAIRY_PROXMOX_TOKEN_SECRET"),
		InsecureCookies:    truthy(os.Getenv("FAIRY_INSECURE_COOKIES")),
	}
	if p := SessionSecretProblem(s.SessionSecret); p != "" {
		return s, errors.New("FAIRY_SESSION_SECRET is " + p)
	}
	return s, nil
}

// exampleSessionSecrets are the FAIRY_SESSION_SECRET values of every released deploy/fairy.env.example.
// Everybody can read them, so none of them is a secret.
var exampleSessionSecrets = []string{"REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS"}

// newSessionSecret is how to make a real one. It goes into every message about a refused value.
const newSessionSecret = "Make a real one with `openssl rand -base64 48`, write it into fairy.env as " +
	"FAIRY_SESSION_SECRET=<that value> and restart the panel (everyone signs in once more)"

// SessionSecretProblem says what is wrong with a FAIRY_SESSION_SECRET value, as the rest of a sentence that
// starts "FAIRY_SESSION_SECRET is" ("" when nothing is). The panel does not start with such a value, and
// `fairy admin config check` reports it. The value itself is never part of the answer.
//
// The secret signs the session cookies: whoever knows it can sign a cookie. So besides the length two exact
// things are refused (T-05), and nothing is guessed about how random a value looks:
//   - the example value of deploy/fairy.env.example (of any release), also inside quotes or with something
//     added to it;
//   - one character repeated.
func SessionSecretProblem(v string) string {
	if len(v) < 32 {
		return "missing or shorter than 32 characters"
	}
	for _, ex := range exampleSessionSecrets {
		if strings.Contains(v, ex) {
			return "still the example value from fairy.env.example, which everybody can read. " + newSessionSecret
		}
	}
	if strings.Count(v, v[:1]) == len(v) {
		return "one character repeated, which is no secret. " + newSessionSecret
	}
	return ""
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
