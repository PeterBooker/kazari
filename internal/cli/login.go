package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/PeterBooker/kazari/internal/apiclient"
)

// LoginCmd saves the API URL and a key once /me accepts them.
type LoginCmd struct{}

func (c *LoginCmd) Run(r *runner) error {
	path, _ := r.configPath()
	if path == "" {
		return usagef("login: no configuration file: set HOME, XDG_CONFIG_HOME or --client-config")
	}
	file, err := readConfigMap(path)
	if err != nil {
		return err
	}
	raw := r.g.URL
	if raw == "" {
		raw = r.env("KAZARI_URL")
	}
	if raw == "" {
		raw, _ = file["url"].(string)
	}
	apiURL, err := normalizeURL(raw)
	if err != nil {
		return err
	}
	if apiURL == "" {
		return usagef("login: give the API URL: kazari login --url https://api.<base>")
	}
	key := r.g.Token
	if key == "" {
		if key, err = r.readKey(apiURL); err != nil {
			return err
		}
	}
	if key == "" {
		return usagef("login: no key given; create one on https://%s/account/", baseDomain(apiURL))
	}
	if err := r.checkScheme(apiURL, key); err != nil {
		return err
	}
	r.apiURL = apiURL
	me, err := r.newClient(apiURL, key, "", apiclient.Options{}).Me(r.ctx)
	if err != nil {
		return err
	}
	file["url"], file["token"] = apiURL, key
	if err := writeConfigMap(path, file); err != nil {
		return err
	}
	role := me.Role
	if role == "" {
		role = "member"
	}
	fmt.Fprintf(termSafe{r.stdout}, "logged in to %s as %s (%s); saved in %s\n", apiURL, me.Handle, role, path)
	if v := r.env("KAZARI_TOKEN"); v != "" && v != key {
		fmt.Fprintln(r.stderr, "note: KAZARI_TOKEN is set and is used before the saved key")
	}
	return nil
}

// readKey prompts on a terminal, with the typing hidden where the terminal
// allows, and otherwise reads the first line of stdin.
func (r *runner) readKey(apiURL string) (string, error) {
	var line string
	read := func() error {
		s, err := r.readLine(bufio.NewReader(r.stdin))
		if err != nil {
			return fmt.Errorf("login: read the key: %w", err)
		}
		line = strings.TrimSpace(s)
		return nil
	}
	if !r.terminal() {
		return line, read()
	}
	fmt.Fprintf(r.stderr, "API key (create one on https://%s/account/): ", baseDomain(apiURL))
	err := withoutEcho(r.stdin, read)
	fmt.Fprintln(r.stderr)
	return line, err
}

// LogoutCmd removes the key from the client configuration.
type LogoutCmd struct{}

func (c *LogoutCmd) Run(r *runner) error {
	path, _ := r.configPath()
	if path == "" {
		return usagef("logout: no configuration file: set HOME, XDG_CONFIG_HOME or --client-config")
	}
	file, err := readConfigMap(path)
	if err != nil {
		return err
	}
	if _, ok := file["token"]; !ok {
		fmt.Fprintf(r.stdout, "no key saved in %s\n", path)
		return nil
	}
	delete(file, "token")
	if err := writeConfigMap(path, file); err != nil {
		return err
	}
	fmt.Fprintf(r.stdout, "removed the key from %s; it still works until it is revoked (kazari keys revoke)\n", path)
	if r.env("KAZARI_TOKEN") != "" {
		fmt.Fprintln(r.stderr, "note: KAZARI_TOKEN is still set")
	}
	return nil
}

// readConfigMap reads the client configuration as a map, so login and
// logout keep the keys they do not change; a missing file is empty.
func readConfigMap(path string) (map[string]any, error) {
	m := map[string]any{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, usagef("config %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, usagef("config %s: %v", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// writeConfigMap replaces the file with one only its owner can read, by
// renaming a new file over it.
func writeConfigMap(path string, m map[string]any) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
