package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/StarMoe-org/ferry/internal/ferry"
	"github.com/StarMoe-org/ferry/internal/server"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}

	var (
		configPath = flag.String("config", "ferry.yaml", "path to configuration YAML file")
		demoMode   = flag.Bool("demo", false, "start with in-memory fake clusters and sample data")
		listenFlag = flag.String("listen", "", "override listen address (e.g. :8080)")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var eng *ferry.Engine
	if *demoMode {
		addr := *listenFlag
		if addr == "" {
			addr = ":8080"
		}
		demo, err := ferry.StartDemo(ctx, addr)
		if err != nil {
			log.Fatalf("demo init error: %v", err)
		}
		defer demo.Close()
		eng = demo.Engine
		log.Printf("ferry %s starting in -demo mode (3 fake clusters: de, sg, jp; auth disabled)", version)
	} else {
		cfg, err := ferry.LoadConfig(*configPath)
		if err != nil {
			log.Fatalf("config error: %v", err)
		}
		if *listenFlag != "" {
			cfg.Listen = *listenFlag
		}
		var initErr error
		eng, initErr = ferry.NewFromConfig(ctx, cfg)
		if initErr != nil {
			log.Fatalf("init error: %v", initErr)
		}
		defer eng.Close()
	}

	srvHandler, err := server.New(eng, server.Options{})
	if err != nil {
		log.Fatalf("server init error: %v", err)
	}

	httpSrv := &http.Server{
		Addr:              eng.Config().Listen,
		Handler:           srvHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("ferry %s listening on %s", version, eng.Config().Listen)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen error: %v", err)
	}
}
