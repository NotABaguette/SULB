package entry

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/buffer"
	"github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/adapter"
	"github.com/xjasonlyu/tun2socks/v2/core/device"
	"github.com/xjasonlyu/tun2socks/v2/core/device/tun"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"sulb/internal/config"
	"sulb/internal/pick"
)

// Stack owns the TUN device and gVisor netstack, terminating TCP/UDP flows
// and dialing each through the picker's chosen link.
type Stack struct {
	device device.Device
	stack  *stack.Stack
}

// StartTun creates the TUN device, brings it up with the configured address
// (OS-specific), and starts the netstack.
func StartTun(cfg config.EntryConfig, p *pick.Picker) (*Stack, error) {
	dev, err := tun.Open(cfg.TUNName, cfg.MTU)
	if err != nil {
		return nil, err
	}
	st, err := core.CreateStack(&core.Config{
		LinkEndpoint:     dev,
		TransportHandler: &tunHandler{p: p, log: slog.Default()},
	})
	if err != nil {
		dev.Close()
		return nil, err
	}
	if err := setupAddress(cfg); err != nil {
		dev.Close()
		st.Close()
		return nil, err
	}
	slog.Info("tun up", "name", cfg.TUNName, "ip", cfg.TUNIP)
	return &Stack{device: dev, stack: st}, nil
}

func (s *Stack) Close() error {
	s.device.Close()
	s.stack.Close()
	return nil
}

type tunHandler struct {
	p   *pick.Picker
	log *slog.Logger
}

func (h *tunHandler) HandleTCP(conn adapter.TCPConn) {
	defer conn.Close()
	id := conn.ID()
	dst := netip.AddrPortFrom(addrFromTcpAddr(id.LocalAddress), id.LocalPort)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	l := h.p.Pick()
	if l == nil {
		return
	}
	rc, err := l.DialContext(ctx, dst)
	if err != nil {
		h.log.Warn("tcp dial failed", "link", l.Name(), "dst", dst.String(), "err", err)
		return
	}
	defer rc.Close()
	l.RecordPassive(time.Since(start))
	Pipe(conn, rc)
}

func (h *tunHandler) HandleUDP(uc adapter.UDPConn) {
	defer uc.Close()
	id := uc.ID()
	dst := netip.AddrPortFrom(addrFromTcpAddr(id.LocalAddress), id.LocalPort)
	l := h.p.Pick()
	if l == nil {
		return
	}
	pc, err := l.DialUDP(dst)
	if err != nil {
		h.log.Warn("udp dial failed", "link", l.Name(), "dst", dst.String(), "err", err)
		return
	}
	defer pc.Close()
	pipePacket(uc, pc, dst)
}

// pipePacket pumps UDP between the netstack endpoint and the link's packet
// conn, dropping replies not from the destination (symmetric-NAT filtering).
func pipePacket(origin, remote net.PacketConn, dst netip.AddrPort) {
	timeout := 5 * time.Minute
	done := make(chan struct{}, 2)
	go func() { copyPacket(origin, remote, timeout, dst); done <- struct{}{} }()
	go func() { copyPacket(remote, origin, timeout, dst); done <- struct{}{} }()
	<-done
	<-done
}

func copyPacket(dst, src net.PacketConn, timeout time.Duration, target netip.AddrPort) {
	buf := buffer.Get(buffer.MaxSegmentSize)
	defer buffer.Put(buf)
	for {
		src.SetReadDeadline(time.Now().Add(timeout))
		n, from, err := src.ReadFrom(buf)
		if err != nil {
			return
		}
		// Only forward replies that came from where we sent.
		if from != nil && from.String() != target.String() {
			continue
		}
		if _, err := dst.WriteTo(buf[:n], netAddrFromAddrPort(target)); err != nil {
			return
		}
	}
}

func addrFromTcpAddr(a tcpip.Address) netip.Addr {
	if a.Len() == 4 {
		return netip.AddrFrom4(a.As4())
	}
	return netip.AddrFrom16(a.As16())
}
