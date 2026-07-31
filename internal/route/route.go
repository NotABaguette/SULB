// Package route re-points OS routes so L3 traffic flows via the picker's
// chosen router link. Linux is first-class; macOS/Windows best-effort.
package route

import (
	"context"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"sulb/internal/links"
	"sulb/internal/pick"
)

// Runner executes an OS command. Injected for tests.
type Runner func(ctx context.Context, name string, args ...string) error

// OS returns the real exec-based runner.
func OS() Runner {
	return func(ctx context.Context, name string, args ...string) error {
		return exec.CommandContext(ctx, name, args...).Run()
	}
}

type Manager struct {
	picker *pick.Picker
	routes []string
	run    Runner
	mu     sync.Mutex
	last   *links.Link
	log    *slog.Logger
}

func NewManager(p *pick.Picker, routes []string, run Runner) *Manager {
	if run == nil {
		run = OS()
	}
	return &Manager{picker: p, routes: routes, run: run, log: slog.Default()}
}

// Run applies the current best router link's routes every 5s.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.apply(ctx)
		}
	}
}

func (m *Manager) apply(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	best := m.picker.Pick()
	if best == nil || best == m.last {
		return
	}
	for _, r := range m.routes {
		if err := SetVia(ctx, m.run, r, best.Cfg().Gateway); err != nil {
			m.log.Error("route apply failed", "prefix", r, "gateway", best.Cfg().Gateway, "err", err)
			best.SetRoutingOK(false)
			return
		}
	}
	best.SetRoutingOK(true)
	m.log.Info("routes repointed", "via", best.Name(), "gateway", best.Cfg().Gateway)
	m.last = best
}
