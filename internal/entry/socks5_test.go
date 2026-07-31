package entry

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"sulb/internal/config"
	"sulb/internal/links"
	"sulb/internal/pick"
	"sulb/internal/score"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy/socks5"
)

func testPicker(t *testing.T) *pick.Picker {
	t.Helper()
	l, err := links.New(config.LinkConfig{Name: "direct", Type: "direct"}, 0.3, score.Norm{
		LatencyBest: 10 * time.Millisecond, LatencyWorst: 300 * time.Millisecond, BandwidthCap: 10 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pick.New([]*links.Link{l}, 0.1, 15*time.Second)
}

func startEchoTCP(t *testing.T) net.Addr {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						c.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr()
}

func startEchoUDP(t *testing.T) net.Addr {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], addr)
		}
	}()
	t.Cleanup(func() { pc.Close() })
	return pc.LocalAddr()
}

func TestSocksConnectRoundtrip(t *testing.T) {
	echoAddr := startEchoTCP(t)
	s, err := NewSocksServer("127.0.0.1:0", testPicker(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client, err := socks5.New(s.Addr().String(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	ec := echoAddr.(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.DialContext(ctx, &M.Metadata{Network: M.TCP, DstIP: netip.MustParseAddr("127.0.0.1"), DstPort: uint16(ec.Port)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := []byte("hello through socks")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := c.Read(got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo mismatch: %q", got)
	}
}

func TestSocksUDPAssociate(t *testing.T) {
	echoAddr := startEchoUDP(t)
	s, err := NewSocksServer("127.0.0.1:0", testPicker(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client, err := socks5.New(s.Addr().String(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	ep := echoAddr.(*net.UDPAddr)
	uc, err := client.DialUDP(&M.Metadata{Network: M.UDP, DstIP: netip.MustParseAddr("127.0.0.1"), DstPort: uint16(ep.Port)})
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	if _, err := uc.WriteTo([]byte("udp-ping"), &net.UDPAddr{IP: netip.MustParseAddr("127.0.0.1").AsSlice(), Port: ep.Port}); err != nil {
		t.Fatal(err)
	}
	uc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := uc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "udp-ping" {
		t.Fatalf("udp echo mismatch: %q", buf[:n])
	}
}
