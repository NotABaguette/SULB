//go:build !linux && !darwin && !windows

package entry

import (
	"fmt"

	"sulb/internal/config"
)

func setupAddress(cfg config.EntryConfig) error {
	return fmt.Errorf("tun address setup unsupported on this platform")
}
