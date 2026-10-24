// Command agalma is the Agalma server (agalma serve) and its client CLI.
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

	"agalma/internal/app"
	"agalma/internal/buildinfo"
	"agalma/internal/cli"
	"agalma/internal/config"
)

// defaultConfigPath is read by serve when neither --config nor
// $AGALMA_CONFIG names a file and it exists.
const defaultConfigPath = "/etc/agalma/agalma.yaml"

// defaultEnvFile is loaded when neither --env-file nor $AGALMA_ENV_FILE
// names a file and it exists in the working directory.
const defaultEnvFile = ".env"

const usage = `usage: agalma <command> [arguments]

Server:
  serve [--config PATH] [--env-file PATH]
                          run the server (config: --config, $AGALMA_CONFIG,
                          /etc/agalma/agalma.yaml, else environment only;
                          secrets: --env-file, $AGALMA_ENV_FILE, else ./.env)
  version                 print the version

Client (run "agalma help" for the full list and global flags):
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
		fmt.Printf("agalma %s (%s, %s, %s)\n", i.Version, i.Commit, i.Date, i.Go)
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

// serve runs the server and returns the exit code: 2 for a configuration
// error, 1 for a runtime error.
func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	path := fs.String("config", "", "config file (default $AGALMA_CONFIG, then "+defaultConfigPath+" if it exists)")
	envFile := fs.String("env-file", "", "dotenv file with secrets (default $AGALMA_ENV_FILE, then ./"+defaultEnvFile+" if it exists)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "agalma: serve: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	cfg, err := config.Load(configPath(*path), envFilePath(*envFile))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agalma: %v\n", err)
		return 2
	}
	logger := cfg.Logger(os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, app.Options{Logger: logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "agalma: %v\n", err)
		return 1
	}
	if err := a.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "agalma: %v\n", err)
		return 1
	}
	return 0
}

// configPath picks the config file: the flag, then $AGALMA_CONFIG, then
// the default path if it exists, else none (environment only).
func configPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if p := os.Getenv("AGALMA_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat(defaultConfigPath); err == nil {
		return defaultConfigPath
	}
	return ""
}

// envFilePath picks the dotenv file: the flag, then $AGALMA_ENV_FILE, else
// "" which lets config.Load use ./.env when it exists.
func envFilePath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("AGALMA_ENV_FILE")
}

// clientEnv is the environment lookup for client commands: the process
// environment first, then ./.env (or $AGALMA_ENV_FILE) when it exists, so a
// local .env can hold AGALMA_URL and AGALMA_TOKEN.
func clientEnv() func(string) (string, bool) {
	path := os.Getenv("AGALMA_ENV_FILE")
	if path == "" {
		path = defaultEnvFile
	}
	dotenv, err := config.LoadEnvFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "agalma: %v\n", err)
		}
		return os.LookupEnv
	}
	return config.EnvLookup(dotenv)
}
