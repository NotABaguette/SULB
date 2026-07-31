//go:build windows

package route

import "context"

// Windows has no replace: try change, fall back to add. Best-effort:
// prefix masks are derived by the OS from the classful address.
func SetVia(ctx context.Context, run Runner, prefix, gw string) error {
	err := run(ctx, "route", "change", prefix, gw)
	if err != nil {
		return run(ctx, "route", "add", prefix, gw)
	}
	return nil
}
