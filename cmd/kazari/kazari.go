// Command kazari is the Kazari server (kazari serve) and its client CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"

	"kazari/internal/app"
	"kazari/internal/buildinfo"
	"kazari/internal/cli"
	"kazari/internal/config"
)

const defaultConfigPath = "/etc/kazari/kazari.yaml"

const defaultEnvFile = ".env"

const usage = `usage: kazari <command> [arguments]

Server:
  serve [--config PATH] [--env-file PATH]
                          run the server (config: --config, $KAZARI_CONFIG,
                          /etc/kazari/kazari.yaml, else environment only;
                          secrets: --env-file, $KAZARI_ENV_FILE, else ./.env)
  version                 print the version

Client (run "kazari help" for the full list and global flags):
  publish, update, get, list, rm, stale, unstale, visibility, rollback,
  files, me, keys, admin
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve(os.Args[2:]))
	case "version", "--version":
		i := buildinfo.Get()
		fmt.Printf("kazari %s (%s, %s, %s)\n", i.Version, i.Commit, i.Date, i.Go)
	case "-h", "--help":
		fmt.Print(usage)
	case "help":
		fmt.Print(usage)
		fmt.Println()
		os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stdout, clientEnv()))
	default:
		os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, clientEnv()))
	}
}

// serve returns exit code 2 for a configuration error, 1 for a runtime error.
func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	path := fs.String("config", "", "config file (default $KAZARI_CONFIG, then "+defaultConfigPath+" if it exists)")
	envFile := fs.String("env-file", "", "dotenv file with secrets (default $KAZARI_ENV_FILE, then ./"+defaultEnvFile+" if it exists)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "kazari: serve: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	cfg, err := config.Load(configPath(*path), envFilePath(*envFile))
	if err != nil {
		fmt.Fprintf(os.Stderr, "kazari: %v\n", err)
		return 2
	}
	logger := cfg.Logger(os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, app.Options{Logger: logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "kazari: %v\n", err)
		return 1
	}
	if err := a.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "kazari: %v\n", err)
		return 1
	}
	return 0
}

func configPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if p := os.Getenv("KAZARI_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat(defaultConfigPath); err == nil {
		return defaultConfigPath
	}
	return ""
}

// envFilePath returns "" when unset so that config.Load falls back to ./.env.
func envFilePath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("KAZARI_ENV_FILE")
}

// clientEnv lets a local .env (or $KAZARI_ENV_FILE) hold KAZARI_URL and
// KAZARI_TOKEN; the process environment wins.
func clientEnv() func(string) (string, bool) {
	path := os.Getenv("KAZARI_ENV_FILE")
	if path == "" {
		path = defaultEnvFile
	}
	dotenv, err := config.LoadEnvFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "kazari: %v\n", err)
		}
		return os.LookupEnv
	}
	return config.EnvLookup(dotenv)
}
