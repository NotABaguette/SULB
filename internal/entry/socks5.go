// Package entry provides the exposed entry points: the SOCKS5 server and the
// TUN stack. Both hand each new flow to the picker.
package entry

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/buffer"

	"sulb/internal/pick"
)

const (
	socksHandshakeTimeout = 30 * time.Second
	socksUDPIdleTimeout   = 5 * time.Minute
)

// SocksServer is a SOCKS5 (no-auth) server whose CONNECT and UDP ASSOCIATE
// requests are dialed through the picker's chosen link.
type SocksServer struct {
	ln     net.Listener
	picker *pick.Picker
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewSocksServer(addr string, p *pick.Picker) (*SocksServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &SocksServer{ln: ln, picker: p, cancel: cancel}
	go s.acceptLoop(ctx)
	return s, nil
}

func (s *SocksServer) Addr() net.Addr { return s.ln.Addr() }

func (s *SocksServer) Start() {} // listener already running; kept for symmetry

func (s *SocksServer) Close() error {
	s.cancel()
	return s.ln.Close()
}

func (s *SocksServer) acceptLoop(ctx context.Context) {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, c)
		}()
	}
}

func (s *SocksServer) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(socksHandshakeTimeout))
	if err := negotiate(c); err != nil {
		return
	}
	cmd, dst, err := readRequest(c)
	if err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	switch cmd {
	case 1: // CONNECT
		s.handleConnect(ctx, c, dst)
	case 3: // UDP ASSOCIATE
		s.handleUDP(ctx, c, dst)
	}
}

func negotiate(c net.Conn) error {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	if hdr[0] != 5 { // SOCKS5 only
		return errBadVersion
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	for _, m := range methods {
		if m == 0x00 { // no-auth
			_, err := c.Write([]byte{5, 0})
			return err
		}
	}
	c.Write([]byte{5, 0xFF}) // no acceptable methods
	return errNoAuth
}

var (
	errBadVersion = errStr("bad socks version")
	errNoAuth     = errStr("no acceptable auth method")
)

type errStr string

func (e errStr) Error() string { return string(e) }

// readRequest returns the command (1=CONNECT, 3=UDP ASSOCIATE) and the
// destination. Hostnames are resolved here — the link dialers take IPs.
func readRequest(c net.Conn) (byte, netip.AddrPort, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return 0, netip.AddrPort{}, err
	}
	if hdr[0] != 5 || hdr[2] != 0 {
		return 0, netip.AddrPort{}, errBadRequest
	}
	var addr netip.Addr
	switch hdr[3] {
	case 1: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return 0, netip.AddrPort{}, err
		}
		addr = netip.AddrFrom4([4]byte(b))
	case 3: // hostname
		b := make([]byte, 1)
		if _, err := io.ReadFull(c, b); err != nil {
			return 0, netip.AddrPort{}, err
		}
		host := make([]byte, int(b[0]))
		if _, err := io.ReadFull(c, host); err != nil {
			return 0, netip.AddrPort{}, err
		}
		ips, err := net.LookupIP(string(host))
		if err != nil || len(ips) == 0 {
			return 0, netip.AddrPort{}, errHostResolve
		}
		addr, _ = netip.AddrFromSlice(ips[0])
		addr = addr.Unmap()
	case 4: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return 0, netip.AddrPort{}, err
		}
		addr = netip.AddrFrom16([16]byte(b))
	default:
		return 0, netip.AddrPort{}, errBadRequest
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return 0, netip.AddrPort{}, err
	}
	return hdr[1], netip.AddrPortFrom(addr, binary.BigEndian.Uint16(pb)), nil
}

var (
	errBadRequest  = errStr("bad socks request")
	errHostResolve = errStr("hostname resolution failed")
)

func writeReply(c net.Conn, code byte) {
	// 5, code, 0, ATYP=1, 0.0.0.0, :0 — clients ignore BND for CONNECT
	c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
}

// writeReplyAddr replies with a real BND address — required for UDP
// ASSOCIATE, where the client sends relay packets to this address.
func writeReplyAddr(c net.Conn, code byte, ap netip.AddrPort) {
	h := []byte{5, code, 0}
	if ap.Addr().Is4() {
		h = append(h, 1)
		b := ap.Addr().As4()
		h = append(h, b[:]...)
	} else {
		h = append(h, 4)
		b := ap.Addr().As16()
		h = append(h, b[:]...)
	}
	h = binary.BigEndian.AppendUint16(h, ap.Port())
	c.Write(h)
}

func (s *SocksServer) handleConnect(ctx context.Context, c net.Conn, dst netip.AddrPort) {
	l := s.picker.Pick()
	if l == nil {
		writeReply(c, 0x01)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rc, err := l.DialContext(dctx, dst)
	if err != nil {
		writeReply(c, 0x04)
		return
	}
	defer rc.Close()
	writeReply(c, 0x00)
	Pipe(c, rc)
}

func (s *SocksServer) handleUDP(ctx context.Context, c net.Conn, dst netip.AddrPort) {
	// Bind the relay on the interface the client reached us on, so the
	// BND.ADDR in the reply is reachable by the client.
	relay, err := net.ListenPacket("udp", c.LocalAddr().String())
	if err != nil {
		writeReply(c, 0x01)
		return
	}
	defer relay.Close()
	l := s.picker.Pick()
	if l == nil {
		writeReply(c, 0x01)
		return
	}
	pc, err := l.DialUDP(dst)
	if err != nil {
		writeReply(c, 0x04)
		return
	}
	defer pc.Close()
	writeReplyAddr(c, 0x00, addrPortFromNetAddr(relay.LocalAddr()))
	s.relayUDP(c, relay, pc, dst)
}

// relayUDP pumps UDP datagrams between the client's relay socket and the
// link's packet conn, adding/removing the SOCKS UDP header (RSV FRAG ATYP
// ADDR PORT). Responses go back to whichever client address sent first.
func (s *SocksServer) relayUDP(c net.Conn, relay, pc net.PacketConn, dst netip.AddrPort) {
	// Shared state between the two pump goroutines: the client's relay
	// source address and the last destination we sent a datagram to.
	var mu sync.Mutex
	var client, lastDst netip.AddrPort
	done := make(chan struct{})
	// The client's TCP session ending is the session-lifetime signal.
	go func() {
		buf := make([]byte, 1)
		c.Read(buf)
		relay.Close()
		pc.Close()
	}()
	go func() { // client -> link
		defer close(done)
		buf := buffer.Get(buffer.MaxSegmentSize)
		defer buffer.Put(buf)
		for {
			relay.SetReadDeadline(time.Now().Add(socksUDPIdleTimeout))
			n, from, err := relay.ReadFrom(buf)
			if err != nil {
				return
			}
			mu.Lock()
			client = addrPortFromNetAddr(from)
			mu.Unlock()
			payload, d, ok := stripUDPHeader(buf[:n], dst)
			if !ok {
				continue
			}
			mu.Lock()
			lastDst = d
			mu.Unlock()
			if _, err := pc.WriteTo(payload, netAddrFromAddrPort(d)); err != nil {
				return
			}
		}
	}()
	go func() { // link -> client
		buf := buffer.Get(buffer.MaxSegmentSize)
		defer buffer.Put(buf)
		for {
			pc.SetReadDeadline(time.Now().Add(socksUDPIdleTimeout))
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			mu.Lock()
			d, cl := lastDst, client
			mu.Unlock()
			// Only accept replies from the address we sent to. The
			// ASSOCIATE request's dst is often 0.0.0.0:0, so compare
			// against the per-packet destination instead.
			if d.IsValid() && from != nil && from.String() != d.String() {
				continue
			}
			if !cl.IsValid() {
				continue
			}
			hdr := makeUDPHeader(cl)
			if _, err := relay.WriteTo(append(hdr, buf[:n]...), netAddrFromAddrPort(cl)); err != nil {
				return
			}
		}
	}()
	<-done
	c.Close()
}

func stripUDPHeader(b []byte, fallback netip.AddrPort) (payload []byte, dst netip.AddrPort, ok bool) {
	if len(b) < 4 {
		return nil, netip.AddrPort{}, false
	}
	if b[2] != 0 { // FRAG must be 0
		return nil, netip.AddrPort{}, false
	}
	switch b[3] {
	case 1:
		if len(b) < 4+4+2 {
			return nil, netip.AddrPort{}, false
		}
		dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), binary.BigEndian.Uint16(b[8:10]))
		return b[10:], dst, true
	case 3:
		if len(b) < 4+1+2 {
			return nil, netip.AddrPort{}, false
		}
		n := int(b[4])
		if len(b) < 4+1+n+2 {
			return nil, netip.AddrPort{}, false
		}
		host := string(b[5 : 5+n])
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return nil, netip.AddrPort{}, false
		}
		addr, _ := netip.AddrFromSlice(ips[0])
		dst = netip.AddrPortFrom(addr.Unmap(), binary.BigEndian.Uint16(b[5+n:7+n]))
		return b[7+n:], dst, true
	case 4:
		if len(b) < 4+16+2 {
			return nil, netip.AddrPort{}, false
		}
		dst = netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[4:20])), binary.BigEndian.Uint16(b[20:22]))
		return b[22:], dst, true
	}
	return nil, netip.AddrPort{}, false
}

func makeUDPHeader(dst netip.AddrPort) []byte {
	h := []byte{0, 0, 0}
	if dst.Addr().Is4() {
		h = append(h, 1)
		b := dst.Addr().As4()
		h = append(h, b[:]...)
	} else {
		h = append(h, 4)
		b := dst.Addr().As16()
		h = append(h, b[:]...)
	}
	return binary.BigEndian.AppendUint16(h, dst.Port())
}

func addrPortFromNetAddr(a net.Addr) netip.AddrPort {
	if u, ok := a.(*net.UDPAddr); ok {
		ap, _ := netip.AddrFromSlice(u.IP)
		return netip.AddrPortFrom(ap.Unmap(), uint16(u.Port))
	}
	return netip.AddrPort{}
}

func netAddrFromAddrPort(ap netip.AddrPort) net.Addr {
	return net.UDPAddrFromAddrPort(ap)
}

// Pipe copies data bidirectionally with half-close support.
func Pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { copyHalf(a, b); done <- struct{}{} }()
	go func() { copyHalf(b, a); done <- struct{}{} }()
	<-done
	<-done
}

func copyHalf(dst, src net.Conn) {
	buf := buffer.Get(buffer.RelayBufferSize)
	defer buffer.Put(buf)
	io.CopyBuffer(dst, src, buf)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	if cr, ok := src.(interface{ CloseRead() error }); ok {
		cr.CloseRead()
	}
}
