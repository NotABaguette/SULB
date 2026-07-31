//go:build windows

package entry

import (
	"fmt"
	"os/exec"

	"sulb/internal/config"
)

func setupAddress(cfg config.EntryConfig) error {
	// Best-effort: wintun adapter naming varies; document.
	if out, err := exec.Command("netsh", "interface", "ipv4", "add", "address",
		cfg.TUNName, cfg.TUNIP, maskString(cfg.TUNNet)).CombinedOutput(); err != nil {
		return fmt.Errorf("netsh add address: %v: %s", err, out)
	}
	return nil
}

func maskString(bits int) string {
	mask := uint32(0xFFFFFFFF) << (32 - bits)
	return fmt.Sprintf("%d.%d.%d.%d", byte(mask>>24), byte(mask>>16), byte(mask>>8), byte(mask))
}
