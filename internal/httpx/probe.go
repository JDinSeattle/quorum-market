package httpx

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// Ping returns a readiness probe that checks a dependency's health endpoint.
//
// It uses its own short-timeout client with no circuit breaker: a probe is
// supposed to report what is actually true right now, and routing it through
// the breaker would make it echo the breaker's own opinion instead.
func Ping(name, url string) obs.Probe {
	client := &http.Client{Timeout: 2 * time.Second}

	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("%s: building probe request: %w", name, err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("%s: unreachable: %w", name, err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("%s: health endpoint returned %d", name, resp.StatusCode)
		}
		return nil
	}
}
