// Command zeroout serves a web UI for overwriting the free space of mounted
// volumes. In Docker, bind-mount each host volume under /targets.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"zeroout/internal/blockdev"
	"zeroout/internal/job"
	"zeroout/internal/web"
)

func main() {
	listen := flag.String("listen", env("ZEROOUT_LISTEN", ":8080"), "HTTP listen address")
	targets := flag.String("targets", env("ZEROOUT_TARGETS", "/targets"), "directory whose subdirectories are the volumes to offer")
	recoverDir := flag.String("recover", env("ZEROOUT_RECOVER", "/recover"), "directory whose subdirectories are recovery destinations")
	data := flag.String("data", env("ZEROOUT_DATA", "/data"), "directory for job state, history and scan results")
	devices := flag.String("devices", os.Getenv("ZEROOUT_DEVICES"), "override scan devices: volume1=/dev/x,volume2=/path/image")
	allowUnmounted := flag.Bool("allow-unmounted", envBool("ZEROOUT_ALLOW_UNMOUNTED"), "allow wiping targets that are not mount points (development only)")
	maxBytes := flag.Int64("max-bytes-per-pass", envInt("ZEROOUT_MAX_BYTES_PER_PASS"), "cap bytes written per pass, 0 for no cap (trial runs)")
	health := flag.Bool("healthcheck", false, "probe the running server and exit (for Docker HEALTHCHECK)")
	flag.Parse()

	if *health {
		os.Exit(healthcheck(*listen))
	}

	m, err := job.NewManager(job.Options{
		TargetsDir:      *targets,
		RecoverDir:      *recoverDir,
		DataDir:         *data,
		Devices:         blockdev.ParseOverrides(*devices),
		AllowUnmounted:  *allowUnmounted,
		MaxBytesPerPass: *maxBytes,
		Logf:            log.Printf,
	})
	if err != nil {
		log.Fatal(err)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           web.Handler(m, os.Getenv("ZEROOUT_PASSWORD")),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Print("shutting down; stopping any running wipe and removing its fill files")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := m.Shutdown(sctx); err != nil {
			log.Printf("wipe cleanup: %v", err)
		}
		srv.Shutdown(sctx)
	}()

	log.Printf("ZeroOut listening on %s (targets %s, data %s)", *listen, *targets, *data)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func healthcheck(listen string) int {
	port := listen[strings.LastIndex(listen, ":")+1:]
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/api/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	b, _ := strconv.ParseBool(os.Getenv(key))
	return b
}

func envInt(key string) int64 {
	n, _ := strconv.ParseInt(os.Getenv(key), 10, 64)
	return n
}
