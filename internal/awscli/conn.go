// Package awscli runs the user's own aws CLI. cloudundo uses the CLI
// instead of an SDK so it inherits exactly the credentials, profiles and
// SSO sessions the agent's command would use.
package awscli

import (
	"encoding/json"
	"sort"
)

// Conn is which account and region a command talks to.
type Conn struct {
	Profile  string `json:"profile,omitempty"`
	Region   string `json:"region,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	// Env holds non-secret AWS_* variables set on the agent's command.
	Env map[string]string `json:"env,omitempty"`
	// secret holds inline credentials. It is used while protecting and is
	// never written to the journal.
	secret map[string]string
}

// safeEnv are the variables worth keeping on disk.
var safeEnv = map[string]bool{
	"AWS_PROFILE": true, "AWS_DEFAULT_PROFILE": true,
	"AWS_REGION": true, "AWS_DEFAULT_REGION": true,
	"AWS_ENDPOINT_URL": true, "AWS_ENDPOINT_URL_S3": true,
	"AWS_ENDPOINT_URL_DYNAMODB": true, "AWS_CONFIG_FILE": true,
	"AWS_SHARED_CREDENTIALS_FILE": true,
}

var secretEnv = map[string]bool{
	"AWS_ACCESS_KEY_ID": true, "AWS_SECRET_ACCESS_KEY": true, "AWS_SESSION_TOKEN": true,
}

// ConnFromEnv keeps the AWS variables of a parsed command: safe ones for
// the journal, credentials only in memory.
func ConnFromEnv(env map[string]string) Conn {
	var c Conn
	for k, v := range env {
		switch {
		case safeEnv[k]:
			if c.Env == nil {
				c.Env = map[string]string{}
			}
			c.Env[k] = v
		case secretEnv[k]:
			if c.secret == nil {
				c.secret = map[string]string{}
			}
			c.secret[k] = v
		}
	}
	return c
}

// FillEnv adds the AWS settings the command inherits from the agent's
// environment (a profile, a region) that the command line does not set
// itself, so undo later talks to the same account and region even from a
// shell set up differently. Credentials are never kept.
func (c *Conn) FillEnv(getenv func(string) string) {
	for k := range safeEnv {
		if _, set := c.Env[k]; set {
			continue
		}
		if v := getenv(k); v != "" {
			if c.Env == nil {
				c.Env = map[string]string{}
			}
			c.Env[k] = v
		}
	}
}

// HasInlineCredentials reports whether the command carried its own keys.
func (c Conn) HasInlineCredentials() bool { return len(c.secret) > 0 }

// environ returns KEY=value pairs to add to the process environment.
func (c Conn) environ() []string {
	var out []string
	for _, m := range []map[string]string{c.Env, c.secret} {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, k+"="+m[k])
		}
	}
	return out
}

// globalArgs are the aws CLI flags that select account and region.
func (c Conn) globalArgs() []string {
	var out []string
	if c.Profile != "" {
		out = append(out, "--profile", c.Profile)
	}
	if c.Region != "" {
		out = append(out, "--region", c.Region)
	}
	if c.Endpoint != "" {
		out = append(out, "--endpoint-url", c.Endpoint)
	}
	return out
}

// Key identifies a Conn for de-duplication.
func (c Conn) Key() string {
	k := c.Profile + "|" + c.Region + "|" + c.Endpoint
	for _, e := range c.environ() {
		k += "|" + e
	}
	return k
}

// jsonMarshal is encoding/json, here so tests can check what reaches disk.
var jsonMarshal = json.Marshal

// Environ is the environment a program run for this Conn needs: the
// command's AWS variables, and inline credentials kept only in memory.
func (c Conn) Environ() []string { return c.environ() }
