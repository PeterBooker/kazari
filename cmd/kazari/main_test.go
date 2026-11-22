package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/cli"
)

const runMainEnv = "KAZARI_TEST_RUN_MAIN"

// TestMain lets the test binary run as kazari itself: main ends in os.Exit,
// so what it does with the exit code and the standard streams shows only
// from outside the process.
func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
	}
	m.Run()
}

// runMain runs main in a child process with this process's environment.
func runMain(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], args...)
	cmd.Env = append(os.Environ(), runMainEnv+"=1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
		t.Fatalf("kazari %s did not run: %v", strings.Join(args, " "), err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}

func TestMainExitsWithTheCommandsResult(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOME", t.TempDir())
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"invalid or expired token"}}`))
	}))
	t.Cleanup(refusing.Close)

	tests := []struct {
		name string
		args []string
		env  map[string]string
		code int
	}{
		{name: "a command that succeeds", args: []string{"version"}, code: 0},
		{name: "a command that does not exist", args: []string{"no-such-command"}, code: 2},
		{
			name: "a command the server refuses",
			args: []string{"me"},
			env:  map[string]string{"KAZARI_URL": refusing.URL, "KAZARI_TOKEN": "dummy-token"},
			code: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			var wantOut, wantErr bytes.Buffer
			wantCode := cli.Run(tt.args, strings.NewReader(""), &wantOut, &wantErr, clientEnv())
			if wantCode != tt.code {
				t.Fatalf("cli.Run(%q) = %d, want %d: the row no longer tests this exit code", tt.args, wantCode, tt.code)
			}

			code, stdout, stderr := runMain(t, tt.args...)
			if code != tt.code {
				t.Errorf("kazari %s exited %d, want %d", strings.Join(tt.args, " "), code, tt.code)
			}
			if stdout != wantOut.String() {
				t.Errorf("kazari %s wrote to stdout:\n%s\nwant what the command wrote:\n%s", strings.Join(tt.args, " "), stdout, wantOut.String())
			}
			if stderr != wantErr.String() {
				t.Errorf("kazari %s wrote to stderr:\n%s\nwant what the command wrote:\n%s", strings.Join(tt.args, " "), stderr, wantErr.String())
			}
			if (tt.code == 0) != (stderr == "") {
				t.Errorf("kazari %s exited %d with stderr %q, want an error message exactly when it fails", strings.Join(tt.args, " "), code, stderr)
			}
		})
	}
}

// captureStderr points os.Stderr, which clientEnv writes to, at a file until
// the test ends. The function it returns reads what was written.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = old
		_ = f.Close()
	})
	return func() string {
		t.Helper()
		data, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
}

func TestClientEnvFile(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		// file is the content of the named file; none is written when empty.
		file string
		env  string
		want string
		// stderr is text the warning has to hold; none means no warning.
		stderr string
	}{
		{name: "the named file supplies a value", file: "KAZARI_URL=https://file.test\n", want: "https://file.test"},
		{name: "the environment wins over the named file", file: "KAZARI_URL=https://file.test\n", env: "https://env.test", want: "https://env.test"},
		{name: "a named file that does not exist", env: "https://env.test", want: "https://env.test"},
		{
			name: "a named file that is not dotenv",
			file: "KAZARI_URL=https://file.test\nnot a pair\n",
			env:  "https://env.test", want: "https://env.test",
			stderr: "line 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-")+".env")
			if tt.file != "" {
				writeFile(t, path, tt.file)
			}
			t.Setenv("KAZARI_ENV_FILE", path)
			if tt.env != "" {
				t.Setenv("KAZARI_URL", tt.env)
			}
			stderr := captureStderr(t)

			got, _ := clientEnv()("KAZARI_URL")
			if got != tt.want {
				t.Errorf("clientEnv()(%q) = %q, want %q", "KAZARI_URL", got, tt.want)
			}
			switch warning := stderr(); {
			case tt.stderr == "" && warning != "":
				t.Errorf("clientEnv() wrote %q to stderr, want nothing", warning)
			case tt.stderr != "" && (!strings.Contains(warning, tt.stderr) || !strings.Contains(warning, path)):
				t.Errorf("clientEnv() wrote %q to stderr, want the file %s and %q", warning, path, tt.stderr)
			}
		})
	}
}
