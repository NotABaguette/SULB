//go:build darwin

package route

import "context"

// macOS has no replace: try change, fall back to add.
func SetVia(ctx context.Context, run Runner, prefix, gw string) error {
	err := run(ctx, "route", "-n", "change", "-net", prefix, gw)
	if err != nil {
		return run(ctx, "route", "-n", "add", "-net", prefix, gw)
	}
	return nil
}
