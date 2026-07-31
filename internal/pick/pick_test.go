package pick

import (
	"testing"
	"time"

	"sulb/internal/config"
	"sulb/internal/links"
	"sulb/internal/score"
)

func mk(name string) *links.Link {
	l, err := links.New(config.LinkConfig{Name: name, Type: "direct"}, 0.3, score.Norm{
		LatencyBest: 10 * time.Millisecond, LatencyWorst: 300 * time.Millisecond, BandwidthCap: 10 << 20,
	})
	if err != nil {
		panic(err)
	}
	return l
}

// drive sets a link's score by feeding UpdateScore metrics, exactly like the
// probe engine does.
func drive(l *links.Link, latency time.Duration) {
	l.UpdateScore(score.Metrics{Latency: latency, LatencyValid: true, Loss: 0})
}

func down(l *links.Link) {
	for i := 0; i < 3; i++ {
		l.RecordProbe(false, 0)
	}
}

func TestInitialPickIsHighestScore(t *testing.T) {
	a, b := mk("a"), mk("b")
	drive(a, 10*time.Millisecond) // 100
	drive(b, 100*time.Millisecond)
	p := New([]*links.Link{a, b}, 0.10, 15*time.Second)
	if got := p.Pick(); got != a {
		t.Fatalf("initial pick: want a, got %s", got.Name())
	}
}

func TestHysteresisPreventsFlap(t *testing.T) {
	a, b := mk("a"), mk("b")
	drive(a, 10*time.Millisecond) // 100
	drive(b, 30*time.Millisecond) // ~93
	p := New([]*links.Link{a, b}, 0.10, 15*time.Second)
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	if got := p.Pick(); got != a {
		t.Fatalf("want a, got %s", got.Name())
	}
	// Oscillate: b slightly better, then a slightly better. Both within
	// the 10% margin of the other -> must stay on a the whole time.
	for i := 0; i < 10; i++ {
		now = now.Add(2 * time.Second)
		if i%2 == 0 {
			drive(b, 20*time.Millisecond)
			drive(a, 10*time.Millisecond)
		} else {
			drive(a, 20*time.Millisecond)
			drive(b, 10*time.Millisecond)
		}
		if got := p.Pick(); got != a {
			t.Fatalf("flapped to %s on iteration %d", got.Name(), i)
		}
	}
}

func TestStickTimeBlocksSwitch(t *testing.T) {
	a, b := mk("a"), mk("b")
	drive(a, 10*time.Millisecond)
	drive(b, 10*time.Millisecond)
	p := New([]*links.Link{a, b}, 0.01, 15*time.Second)
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	if got := p.Pick(); got != a {
		t.Fatalf("want a, got %s", got.Name())
	}
	// b becomes clearly better but stick time hasn't elapsed.
	drive(b, 10*time.Millisecond)
	drive(a, 10*time.Second)
	if got := p.Pick(); got != a {
		t.Fatalf("stick time should hold a: got %s", got.Name())
	}
	// After 20s (past 15s stick) it may switch.
	now = now.Add(20 * time.Second)
	if got := p.Pick(); got != b {
		t.Fatalf("after stick time want b, got %s", got.Name())
	}
}

func TestFailoverIsImmediate(t *testing.T) {
	a, b := mk("a"), mk("b")
	drive(a, 10*time.Millisecond)
	drive(b, 50*time.Millisecond)
	p := New([]*links.Link{a, b}, 0.10, time.Hour) // long stick
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	if got := p.Pick(); got != a {
		t.Fatalf("want a, got %s", got.Name())
	}
	down(a) // a goes Down regardless of stick time
	if got := p.Pick(); got != b {
		t.Fatalf("failover must ignore stick time: want b, got %s", got.Name())
	}
}

func TestAllDownFailClosed(t *testing.T) {
	a, b := mk("a"), mk("b")
	drive(a, 10*time.Millisecond)
	drive(b, 200*time.Millisecond)
	p := New([]*links.Link{a, b}, 0.10, 15*time.Second)
	down(a)
	down(b)
	got := p.Pick()
	if got == nil || got != a { // least-bad (higher score) still returned
		t.Fatalf("fail-closed should return least-bad link, got %v", got)
	}
	if s := got.Snapshot(); s.State != links.StateDown {
		t.Fatalf("picked link must still report Down, got %v", s.State)
	}
}
