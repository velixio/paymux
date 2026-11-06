package hub

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/velixio/paymux/internal/logging"
	"github.com/velixio/paymux/internal/settings"
)

// terminalSettingsEvery is how often the terminal settings are looked at. The
// settings cache is cleared by a change on this node, so a change here shows by
// the next look; another node sees it once its cache has expired too.
const terminalSettingsEvery = time.Second

// applyTerminalSettings sets the terminal to the stored values. The terminal
// belongs to the whole process, so it follows the first organisation's settings.
func applyTerminalSettings(ctx context.Context, set *settings.Service, term *logging.Terminal) error {
	var name string
	var requests bool
	for key, dst := range map[string]any{"terminal_log_level": &name, "terminal_log_requests": &requests} {
		v, err := set.Resolve(ctx, "", key)
		if err != nil {
			return err
		}
		if err := v.Decode(dst); err != nil {
			return err
		}
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(name)); err != nil {
		return err
	}
	term.SetLevel(level)
	term.SetRequests(requests)
	return nil
}

// followTerminalSettings applies the settings now and keeps them applied until
// the returned function is called; it returns once the loop has ended, so call
// it before the database closes. A logger given through WithLogger belongs to
// the embedding program and is left alone.
func (a *App) followTerminalSettings(ctx context.Context, set *settings.Service) (stop func()) {
	if a.term == nil {
		return func() {}
	}
	tick := time.NewTicker(terminalSettingsEvery)
	stopLoop := a.followTerminalTicks(ctx, set, tick.C)
	return func() { stopLoop(); tick.Stop() }
}

// followTerminalTicks is followTerminalSettings, looking at each value of tick.
func (a *App) followTerminalTicks(ctx context.Context, set *settings.Service, tick <-chan time.Time) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var lastErr string
	apply := func() {
		err := applyTerminalSettings(ctx, set, a.term)
		switch {
		case err == nil:
			lastErr = ""
		case errors.Is(err, context.Canceled):
		case err.Error() != lastErr: // once per failure, not once per second
			lastErr = err.Error()
			a.log.Warn("could not read the terminal log settings; keeping the current ones", "err", lastErr)
		}
	}
	apply()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
				apply()
			}
		}
	}()
	return func() { cancel(); <-done }
}
