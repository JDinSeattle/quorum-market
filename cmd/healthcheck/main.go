// Command healthcheck probes a URL and exits 0 when it answers 2xx.
//
// It exists because the service images are distroless: there is no shell, no
// curl and no wget, so a container healthcheck has nothing to run. Shipping a
// tiny static prober keeps the images minimal — no package manager, no shell
// to be exploited — while still letting the orchestrator know when a container
// is actually serving rather than merely running.
//
//	healthcheck http://localhost:8081/product/health
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: healthcheck <url>")
		os.Exit(2)
	}
	url := os.Args[1]

	timeout := 3 * time.Second
	if raw := os.Getenv("HEALTHCHECK_TIMEOUT"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			timeout = parsed
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		os.Exit(1)
	}
}
