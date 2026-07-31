// Package pick selects the best link for each new flow with hysteresis and
// stick time so links never flap.
package pick

import (
	"log/slog"
	"sync"
	"time"

	"sulb/internal/links"
)

type Picker struct {
	mu       sync.Mutex
	links    []*links.Link
	hyst     float64
	stick    time.Duration
	current  *links.Link
	switched time.Time
	now      func() time.Time
	log      *slog.Logger
}

func New(ls []*links.Link, hyst float64, stick time.Duration) *Picker {
	return &Picker{links: ls, hyst: hyst, stick: stick, now: time.Now, log: slog.Default()}
}

// Pick returns the link for a new flow. nil only when no links are configured.
func (p *Picker) Pick() *links.Link {
	p.mu.Lock()
	defer p.mu.Unlock()

	best := bestAmong(p.links, links.StateUp)
	if best == nil {
		// Fail-closed: every link down/degraded -> least-bad one.
		best = bestAmong(p.links, links.StateDegraded)
	}
	if best == nil {
		best = bestAmong(p.links, links.StateDown)
	}
	if best == nil {
		return nil
	}

	cur := p.current
	if cur == nil {
		p.current, p.switched = best, p.now()
		return best
	}
	if cur == best {
		return best
	}

	curSnap, bestSnap := cur.Snapshot(), best.Snapshot()
	// Stick time only protects a healthy current link.
	if curSnap.State == links.StateUp && p.now().Sub(p.switched) < p.stick {
		return cur
	}
	// Hysteresis: only switch when the candidate is clearly better.
	if curSnap.State == links.StateUp && bestSnap.Score <= curSnap.Score*(1+p.hyst) {
		return cur
	}
	p.log.Info("switching link",
		"from", cur.Name(), "to", best.Name(),
		"from_score", round2(curSnap.Score), "to_score", round2(bestSnap.Score))
	p.current, p.switched = best, p.now()
	return best
}

func (p *Picker) Current() *links.Link {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

func bestAmong(ls []*links.Link, s links.State) *links.Link {
	var best *links.Link
	for _, l := range ls {
		if l.Snapshot().State != s {
			continue
		}
		if best == nil || l.Snapshot().Score > best.Snapshot().Score {
			best = l
		}
	}
	return best
}

func round2(x float64) float64 { return float64(int(x*100)) / 100 }
