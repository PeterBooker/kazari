// Command kazari is the Kazari server (kazari serve) and its client CLI.
// Every command lives in internal/cli.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"

	"github.com/PeterBooker/kazari/internal/cli"
	"github.com/PeterBooker/kazari/internal/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// The first signal lets the command stop and say what it left behind;
	// a second one ends the process at once.
	context.AfterFunc(ctx, stop)
	code := cli.Run(ctx, os.Args[1:], processEnv())
	stop()
	os.Exit(code)
}

func processEnv() cli.Env {
	return cli.Env{
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		LookupEnv:     clientEnv(),
		IsTerminal:    cli.IsTerminal,
		DefaultConfig: cli.DefaultConfigPath,
	}
}

// clientEnv lets the dotenv file named by $KAZARI_ENV_FILE hold the client's
// KAZARI_* variables; the process environment wins. The working directory's
// ./.env is never read here: the directory may be someone else's, and a URL
// from it would receive the credentials stored in the user's config file.
func clientEnv() func(string) (string, bool) {
	path := os.Getenv("KAZARI_ENV_FILE")
	if path == "" {
		return os.LookupEnv
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
