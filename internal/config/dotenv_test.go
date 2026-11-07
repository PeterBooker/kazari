package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeEnvFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEnvFile(t *testing.T) {
	tests := []struct {
		name, in string
		want     map[string]string
	}{
		{"plain", "A=1\nB=two words\n", map[string]string{"A": "1", "B": "two words"}},
		{"blank and comments", "\n# comment\n   # indented comment\n\nA=1\n", map[string]string{"A": "1"}},
		{"export prefix", "export A=1\nexport\tB=2\nexport=3\n", map[string]string{"A": "1", "B": "2", "export": "3"}},
		{"spaces around", "  A = value  \n", map[string]string{"A": "value"}},
		{"empty", "A=\nB=   \nC=''\nD=\"\"\n", map[string]string{"A": "", "B": "", "C": "", "D": ""}},
		{"trailing comment", "A=1 # one\nB=x\t# tab\nC=a#b\nD=#notcomment\nE= # only comment\n", map[string]string{"A": "1", "B": "x", "C": "a#b", "D": "#notcomment", "E": ""}},
		{"single quotes literal", `A='a \n $B # c "d"'` + "\n", map[string]string{"A": `a \n $B # c "d"`}},
		{"double quote escapes", `A="l1\nl2\tt\rr \"q\" \\ \x"` + "\n", map[string]string{"A": "l1\nl2\tt\rr \"q\" \\ \\x"}},
		{"double quotes keep hash", `A="a # b" # comment` + "\n", map[string]string{"A": "a # b"}},
		{"quoted with spaces", "A='  padded  '\n", map[string]string{"A": "  padded  "}},
		{"no interpolation", "A=1\nB=${A}\nC=$A\n", map[string]string{"A": "1", "B": "${A}", "C": "$A"}},
		{"equals in value", "A=x=y=z\n", map[string]string{"A": "x=y=z"}},
		{"later wins", "A=1\nA=2\n", map[string]string{"A": "2"}},
		{"crlf", "A=1\r\nB='2'\r\n", map[string]string{"A": "1", "B": "2"}},
		{"bom and no newline", "\xef\xbb\xbfA=1", map[string]string{"A": "1"}},
		{"example file", "KAZARI_AUTH_ADMIN_TOKEN=change-me\nKAZARI_TLS_DNS_API_TOKEN=\n# KAZARI_AUTH_OIDC_CLIENT_SECRET=\n",
			map[string]string{"KAZARI_AUTH_ADMIN_TOKEN": "change-me", "KAZARI_TLS_DNS_API_TOKEN": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadEnvFile(writeEnvFile(t, t.TempDir(), tt.in))
			if err != nil {
				t.Fatalf("LoadEnvFile: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("LoadEnvFile = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadEnvFileErrors(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"no equals", "A=1\nJUST_A_WORD\n", "line 2: want KEY=VALUE"},
		{"bad key", "1A=x\n", "line 1: invalid variable name \"1A\""},
		{"empty key", "=x\n", "line 1: invalid variable name"},
		{"key with dash", "\n\nA-B=x\n", "line 3: invalid variable name \"A-B\""},
		{"unterminated single", "A='x\n", "line 1: A: unterminated single-quoted value"},
		{"unterminated double", "A=\"x\\\"\n", "line 1: A: unterminated double-quoted value"},
		{"text after quote", "A='x' y\n", "line 1: A: unexpected text after the closing quote"},
		{"text glued to quote", "A=\"x\"y\n", "unexpected text after the closing quote"},
		{"export without assignment", "export A\n", "line 1: want KEY=VALUE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeEnvFile(t, t.TempDir(), tt.in)
			_, err := LoadEnvFile(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), path) {
				t.Fatalf("err = %v, want one naming %s and containing %q", err, path, tt.want)
			}
		})
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	_, err := LoadEnvFile(filepath.Join(t.TempDir(), ".env"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestEnvLookup(t *testing.T) {
	t.Setenv("KAZARI_TEST_PROCESS", "process")
	t.Setenv("KAZARI_TEST_EMPTY", "")
	dotenv := map[string]string{
		"KAZARI_TEST_PROCESS": "dotenv",
		"KAZARI_TEST_EMPTY":   "dotenv",
		"KAZARI_TEST_DOTENV":  "dotenv",
	}
	lookup := EnvLookup(dotenv)
	dotenv["KAZARI_TEST_LATER"] = "added after EnvLookup"
	tests := []struct {
		key    string
		want   string
		wantOK bool
	}{
		{"KAZARI_TEST_PROCESS", "process", true},
		{"KAZARI_TEST_EMPTY", "", true},
		{"KAZARI_TEST_DOTENV", "dotenv", true},
		{"KAZARI_TEST_LATER", "", false},
		{"KAZARI_TEST_NOWHERE", "", false},
	}
	for _, tt := range tests {
		if got, ok := lookup(tt.key); got != tt.want || ok != tt.wantOK {
			t.Errorf("lookup(%s) = %q, %v; want %q, %v", tt.key, got, ok, tt.want, tt.wantOK)
		}
	}
	if _, ok := EnvLookup(nil)("KAZARI_TEST_NOWHERE"); ok {
		t.Error("nil dotenv: lookup found a value")
	}
}

const loadTestYAML = `
base_domain: ${KAZARI_TEST_DOMAIN}
data_dir: /d
tls:
  mode: self-signed
`

func TestLoadEnvFileOption(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeYAML(t, loadTestYAML)
	envPath := filepath.Join(dir, "kazari.env")
	if err := os.WriteFile(envPath, []byte("KAZARI_TEST_DOMAIN=from.envfile\nKAZARI_AUTH_ADMIN_TOKEN=adm-0123456789abcdef0123456789ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	writeEnvFile(t, ".", "KAZARI_TEST_DOMAIN=from.dotenv\n")

	c, err := Load(cfgPath, envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.BaseDomain != "from.envfile" {
		t.Errorf("BaseDomain = %q, want from.envfile", c.BaseDomain)
	}
	if c.Auth.AdminToken != testAdminToken {
		t.Errorf("AdminToken = %q, want %s from the KAZARI_* override in the env file", c.Auth.AdminToken, testAdminToken)
	}

	t.Setenv("KAZARI_TEST_DOMAIN", "from.process")
	if c, err := Load(cfgPath, envPath); err != nil || c.BaseDomain != "from.process" {
		t.Errorf("with process env: BaseDomain = %q, %v; want from.process", c.BaseDomain, err)
	}

	missing := filepath.Join(dir, "nope.env")
	if _, err := Load(cfgPath, missing); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), missing) {
		t.Errorf("missing env file: err = %v, want fs.ErrNotExist naming %s", err, missing)
	}
}

func TestLoadDefaultEnvFile(t *testing.T) {
	cfgPath := writeYAML(t, loadTestYAML)
	t.Chdir(t.TempDir())

	_, err := Load(cfgPath, "")
	if want := "${KAZARI_TEST_DOMAIN} is not set"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("without .env: err = %v, want %q", err, want)
	}

	writeEnvFile(t, ".", "KAZARI_TEST_DOMAIN=from.dotenv\n")
	c, err := Load(cfgPath, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.BaseDomain != "from.dotenv" {
		t.Errorf("BaseDomain = %q, want from.dotenv", c.BaseDomain)
	}

	writeEnvFile(t, ".", "NOT A LINE\n")
	if _, err := Load(cfgPath, ""); err == nil || !strings.Contains(err.Error(), ".env: line 1") {
		t.Errorf("malformed .env: err = %v, want a line error", err)
	}
}

// Beside a config file, ./.env fills ${NAME} references and nothing else: it
// is not a source the operator named, so it must not move a credential from
// the file to another destination.
func TestLoadDefaultEnvFileDoesNotOverrideConfigFile(t *testing.T) {
	const yaml = `
base_domain: art.example.test
data_dir: /d
tls:
  mode: self-signed
auth:
  admin_token: ${KAZARI_TEST_ADMIN_TOKEN}
telemetry:
  otlp:
    endpoint: https://otlp.config.test
    headers:
      authorization: Bearer dummy-config-token
`
	const dotenv = "KAZARI_TEST_ADMIN_TOKEN=" + testAdminToken + "\n" +
		"KAZARI_TELEMETRY_OTLP_ENDPOINT=https://otlp.dotenv.test\n" +
		"KAZARI_TELEMETRY_OTLP_HEADERS=authorization=dummy-dotenv\n" +
		"KAZARI_BASE_DOMAIN=dotenv.test\n"
	cfgPath := writeYAML(t, yaml)
	dir := t.TempDir()
	t.Chdir(dir)
	envPath := writeEnvFile(t, dir, dotenv)

	c, err := Load(cfgPath, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Telemetry.OTLP.Endpoint; got != "https://otlp.config.test" {
		t.Errorf("Endpoint = %q from the working directory's dotenv file", got)
	}
	if got := c.Telemetry.OTLP.Headers["authorization"]; got != "Bearer dummy-config-token" {
		t.Errorf("Headers[authorization] = %q from the working directory's dotenv file", got)
	}
	if c.BaseDomain != "art.example.test" {
		t.Errorf("BaseDomain = %q from the working directory's dotenv file", c.BaseDomain)
	}
	if c.Auth.AdminToken != testAdminToken {
		t.Errorf("AdminToken = %q, want the ${NAME} reference filled from the dotenv file", c.Auth.AdminToken)
	}

	// A secret's _FILE variable is an override too.
	t.Run("secret file", func(t *testing.T) {
		secret := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(secret, []byte("authorization=dummy-file"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeEnvFile(t, dir, dotenv+"KAZARI_TELEMETRY_OTLP_HEADERS_FILE="+secret+"\n")
		t.Cleanup(func() { writeEnvFile(t, dir, dotenv) })
		c, err := Load(cfgPath, "")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := c.Telemetry.OTLP.Headers["authorization"]; got != "Bearer dummy-config-token" {
			t.Errorf("Headers[authorization] = %q from a _FILE variable in the dotenv file", got)
		}
	})

	// The same file named on purpose overrides as before.
	t.Run("named file", func(t *testing.T) {
		c, err := Load(cfgPath, envPath)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := c.Telemetry.OTLP.Endpoint; got != "https://otlp.dotenv.test" {
			t.Errorf("Endpoint = %q, want the override from the named file", got)
		}
	})

	t.Run("process environment", func(t *testing.T) {
		t.Setenv("KAZARI_TELEMETRY_OTLP_ENDPOINT", "https://otlp.process.test")
		c, err := Load(cfgPath, "")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := c.Telemetry.OTLP.Endpoint; got != "https://otlp.process.test" {
			t.Errorf("Endpoint = %q, want the process environment's", got)
		}
	})

	// With no config file there is no destination for the dotenv file to
	// move, and it stays the whole configuration.
	t.Run("no config file", func(t *testing.T) {
		writeEnvFile(t, dir, "KAZARI_BASE_DOMAIN=dotenv.test\nKAZARI_DATA_DIR=/d\nKAZARI_TLS_MODE=self-signed\n")
		t.Cleanup(func() { writeEnvFile(t, dir, dotenv) })
		c, err := Load("", "")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.BaseDomain != "dotenv.test" {
			t.Errorf("BaseDomain = %q, want dotenv.test", c.BaseDomain)
		}
	})
}
