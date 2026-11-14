package main

import (
	"context"
	"os"

	"github.com/johandry/vueltario/internal/cli"
	"github.com/johandry/vueltario/internal/config"
)

func main() {
	app := &cli.App{
		Cfg:    config.Load(),
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	os.Exit(app.Run(context.Background(), os.Args[1:]))
}
