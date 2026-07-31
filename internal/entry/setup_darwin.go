//go:build darwin

package entry

import (
	"fmt"
	"net/netip"
	"os/exec"

	"sulb/internal/config"
)

func setupAddress(cfg config.EntryConfig) error {
	ip, err := netip.ParseAddr(cfg.TUNIP)
	if err != nil {
		return err
	}
	bcast := netip.Addr{}
	if ip.Is4() {
		a := ip.As4()
		bcast = netip.AddrFrom4([4]byte{a[0], a[1], a[2], 255})
	}
	// ifconfig utunN 10.66.66.1 10.66.66.255 up
	args := []string{cfg.TUNName, cfg.TUNIP}
	if bcast.IsValid() {
		args = append(args, bcast.String())
	}
	args = append(args, "up")
	if out, err := exec.Command("ifconfig", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ifconfig: %v: %s", err, out)
	}
	return nil
}
