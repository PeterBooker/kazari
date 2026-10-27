// Command kazari is the Kazari server (kazari serve) and its client CLI.
// Every command lives in internal/cli.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/PeterBooker/kazari/internal/cli"
	"github.com/PeterBooker/kazari/internal/config"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, clientEnv()))
}

// clientEnv lets the dotenv file in the working directory (or the one named
// by $KAZARI_ENV_FILE) hold the client's KAZARI_* variables; the process
// environment wins.
func clientEnv() func(string) (string, bool) {
	path := os.Getenv("KAZARI_ENV_FILE")
	if path == "" {
		path = config.DefaultEnvFile
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
