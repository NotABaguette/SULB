// sulb — Simple Uplink Load Balancer. Exposes one IP (TUN + SOCKS5) and
// forwards each new flow through the best uplink by weighted
// latency/loss/bandwidth scoring with EWMA, hysteresis and stick time.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"sulb/internal/config"
	"sulb/internal/entry"
	"sulb/internal/links"
	"sulb/internal/pick"
	"sulb/internal/probe"
	"sulb/internal/route"
	"sulb/internal/score"
	"sulb/internal/status"
)

func main() {
	cfgPath := flag.String("c", "sulb.yaml", "config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config.Config) error {
	norm := score.Norm{
		LatencyBest:  cfg.Scoring.LatencyBest.Duration,
		LatencyWorst: cfg.Scoring.LatencyWorst.Duration,
		BandwidthCap: cfg.Scoring.BandwidthCap,
	}
	ls := make([]*links.Link, 0, len(cfg.Links))
	var socksLinks, routerLinks []*links.Link
	for _, lc := range cfg.Links {
		l, err := links.New(lc, cfg.Scoring.EWMAAlpha, norm)
		if err != nil {
			return err
		}
		ls = append(ls, l)
		if lc.Type == "router" {
			routerLinks = append(routerLinks, l)
		} else {
			socksLinks = append(socksLinks, l)
		}
	}

	pickers := map[string]*pick.Picker{}
	var socksPicker, routerPicker *pick.Picker
	if len(socksLinks) > 0 {
		socksPicker = pick.New(socksLinks, cfg.Scoring.Hysteresis, cfg.Scoring.StickTime.Duration)
		pickers["socks"] = socksPicker
	}
	if len(routerLinks) > 0 {
		routerPicker = pick.New(routerLinks, cfg.Scoring.Hysteresis, cfg.Scoring.StickTime.Duration)
		pickers["router"] = routerPicker
	}

	probe.New(ls, cfg.Scoring).Start(ctx)

	// TUN first so the SOCKS listener can bind to the TUN IP.
	if cfg.Entry.TUNName != "" && socksPicker != nil {
		st, err := entry.StartTun(cfg.Entry, socksPicker)
		if err != nil {
			slog.Warn("tun failed to start", "err", err)
		} else {
			defer st.Close()
		}
	}
	if socksPicker != nil {
		ss, err := entry.NewSocksServer(cfg.Entry.SOCKSListen, socksPicker)
		if err != nil {
			return err
		}
		defer ss.Close()
		slog.Info("socks listening", "addr", ss.Addr().String())
	}

	if routerPicker != nil {
		var allRoutes []string
		for _, rl := range routerLinks {
			allRoutes = append(allRoutes, rl.Cfg().Routes...)
		}
		mgr := route.NewManager(routerPicker, allRoutes, nil)
		go mgr.Run(ctx)
	}

	if srv, err := status.New(cfg.Status.Listen, ls, pickers); err == nil {
		srv.Start()
		defer srv.Close()
		slog.Info("status listening", "addr", cfg.Status.Listen)
	} else {
		slog.Warn("status endpoint failed", "err", err)
	}

	<-ctx.Done()
	slog.Info("shutting down")
	return nil
}
