package route

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"sulb/internal/config"
	"sulb/internal/links"
	"sulb/internal/pick"
	"sulb/internal/score"
)

type fakeRunner struct {
	mu   sync.Mutex
	cmds [][]string
	err  error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, append([]string{name}, args...))
	return f.err
}

func mkLink(t *testing.T, name, typ, gateway string) *links.Link {
	t.Helper()
	l, err := links.New(config.LinkConfig{Name: name, Type: typ, Gateway: gateway}, 0.3, score.Norm{
		LatencyBest: 10 * time.Millisecond, LatencyWorst: 300 * time.Millisecond, BandwidthCap: 10 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSetVia(t *testing.T) {
	var got []string
	run := func(_ context.Context, name string, args ...string) error {
		got = append(got, append([]string{name}, args...)...)
		return nil
	}
	if err := SetVia(context.Background(), run, "0.0.0.0/0", "192.168.1.2"); err != nil {
		t.Fatal(err)
	}
	join := strings.Join(got, " ")
	if !strings.Contains(join, "0.0.0.0/0") || !strings.Contains(join, "192.168.1.2") {
		t.Fatalf("SetVia must pass prefix and gateway, got %q", join)
	}
}

func TestManagerRepointsRoutesOnSwitch(t *testing.T) {
	a := mkLink(t, "a", "router", "192.168.1.2")
	b := mkLink(t, "b", "router", "192.168.1.3")
	drive := func(l *links.Link, lat time.Duration) {
		l.UpdateScore(score.Metrics{Latency: lat, LatencyValid: true, Loss: 0})
	}
	drive(a, 10*time.Millisecond)
	drive(b, 200*time.Millisecond)
	p := pick.New([]*links.Link{a, b}, 0.1, 10*time.Millisecond) // short stick for the test
	fr := &fakeRunner{}
	m := NewManager(p, []string{"0.0.0.0/0"}, fr.Run)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.apply(ctx)
	if len(fr.cmds) != 1 || !strings.Contains(strings.Join(fr.cmds[0], " "), "192.168.1.2") {
		t.Fatalf("want route via a's gateway, got %v", fr.cmds)
	}
	if !a.Snapshot().RoutingOK {
		t.Fatal("a should have RoutingOK after successful apply")
	}
	// b wins now -> next apply repoints to b's gateway (after stick time).
	// EWMA means scores need a few cycles to settle before b overtakes a.
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 3; i++ {
		drive(b, 10*time.Millisecond)
		drive(a, 200*time.Millisecond)
	}
	m.apply(ctx)
	if len(fr.cmds) != 2 || !strings.Contains(strings.Join(fr.cmds[1], " "), "192.168.1.3") {
		t.Fatalf("want route via b's gateway, got %v", fr.cmds)
	}
	// same picker state -> no re-apply
	m.apply(ctx)
	if len(fr.cmds) != 2 {
		t.Fatalf("no-op apply must not re-run routes: %v", fr.cmds)
	}
}

func TestManagerRouteFailureMarksLink(t *testing.T) {
	a := mkLink(t, "a", "router", "192.168.1.2")
	a.UpdateScore(score.Metrics{Latency: 10 * time.Millisecond, LatencyValid: true, Loss: 0})
	p := pick.New([]*links.Link{a}, 0.1, 15*time.Second)
	fr := &fakeRunner{err: errBoom}
	m := NewManager(p, []string{"0.0.0.0/0"}, fr.Run)
	ctx := context.Background()
	m.apply(ctx)
	if a.Snapshot().RoutingOK {
		t.Fatal("link must be marked routing-failed when route command fails")
	}
}

var errBoom = errStr("boom")

type errStr string

func (e errStr) Error() string { return string(e) }
