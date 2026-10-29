package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/StarMoe-org/ferry/internal/ferry"
	"github.com/StarMoe-org/ferry/web"
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
	)
	flag.Parse()

	if _, err := web.Dist(); err != nil {
		log.Fatalf("embedded web assets: %v", err)
	}

	if *demoMode {
		log.Println("demo mode will be wired in server startup")
		return
	}

	cfg, err := ferry.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	log.Printf("ferry %s loaded config (%d clusters, listen %s)", version, len(cfg.Clusters), cfg.Listen)
}
