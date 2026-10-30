package app

import (
	"context"

	"commandcenter/internal/ui"
)

// ExportConfig and ApplyConfig run the web UI's configuration export and
// apply (ui.ExportConfig, ui.ApplyConfig) as the CLI's admin principal
// (ui.WithConfigActor), for "commandcenter config": the same handlers as
// the REST API, on this process's database connection, whose
// notifications make the running nodes apply every change.
func (a *App) ExportConfig(ctx context.Context, opts ui.ConfigOptions) ([]byte, error) {
	return a.ui.ExportConfig(ui.WithConfigActor(ctx), opts)
}

func (a *App) ApplyConfig(ctx context.Context, doc []byte, opts ui.ConfigOptions) (*ui.ConfigResult, error) {
	return a.ui.ApplyConfig(ui.WithConfigActor(ctx), doc, opts)
}
