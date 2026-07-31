//go:build linux

package entry

import (
	"fmt"
	"os/exec"

	"sulb/internal/config"
)

func setupAddress(cfg config.EntryConfig) error {
	if out, err := exec.Command("ip", "addr", "add", fmt.Sprintf("%s/%d", cfg.TUNIP, cfg.TUNNet), "dev", cfg.TUNName).CombinedOutput(); err != nil {
		return fmt.Errorf("ip addr add: %v: %s", err, out)
	}
	if out, err := exec.Command("ip", "link", "set", cfg.TUNName, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("ip link up: %v: %s", err, out)
	}
	return nil
}
