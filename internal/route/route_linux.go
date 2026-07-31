//go:build linux

package route

import "context"

func SetVia(ctx context.Context, run Runner, prefix, gw string) error {
	return run(ctx, "ip", "route", "replace", prefix, "via", gw)
}
