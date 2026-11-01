package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/xartus2004/arena/agent"
	"github.com/xartus2004/arena/episode"
)

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var sf stackFlags
	sf.register(fs)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	_ = fs.Parse(args)

	st, err := openStack(ctx, sf)
	if err != nil {
		return err
	}
	for _, e := range st.envs {
		st.warmFor(e)
	}
	api := agent.NewServer(agent.ServerConfig{
		Envs: st.envs, Episode: st.epCfg, MaxAttempts: sf.attempts, Admission: st.sched, Logger: st.log,
		OnOutcome: func(o *episode.Outcome) {
			st.metrics.ObserveOutcome(o)
			if err := st.store.Save(context.Background(), o, "api"); err != nil {
				st.log.Warn("save outcome", "err", err)
			}
		},
	})
	mux := http.NewServeMux()
	mux.Handle("/metrics", st.metrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("/", api)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "arena: serving on http://%s (runtime=%s, concurrency=%d)\n", *addr, sf.runtime, sf.concurrency)

	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	fmt.Fprintln(os.Stderr, "arena: shutting down, draining episodes...")
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_ = srv.Shutdown(sctx)
	api.Close(sctx)
	cerr := st.close(sctx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return cerr
}
