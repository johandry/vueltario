package main

import (
	"context"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	desktopassets "github.com/johandry/vueltario/desktop"
	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/desktop"
)

func main() {
	cfg := config.Load()
	app := desktop.NewApp(cfg, desktop.NewOSKeyringStore(), nil)

	err := wails.Run(&options.App{
		Title:     "Vueltario Studio — @cubansocial.sd",
		Width:     1320,
		Height:    860,
		MinWidth:  980,
		MinHeight: 680,
		AssetServer: &assetserver.Options{
			Assets: desktopassets.Assets,
		},
		OnStartup: func(ctx context.Context) {
			if err := app.Startup(ctx); err != nil {
				log.Fatalf("vueltario desktop startup error: %v", err)
			}
		},
		OnShutdown: func(ctx context.Context) {
			app.Shutdown(ctx)
		},
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
