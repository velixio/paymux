package dashboard

import (
	"context"

	"github.com/paymux/paymux/internal/selfupdate"
)

// Updater installs a newer release of the hub (*selfupdate.Manager).
type Updater interface {
	Info(ctx context.Context, refresh bool) selfupdate.Info
	Start(target string, force bool, done func(selfupdate.Status)) error
	Status() selfupdate.Status
}
