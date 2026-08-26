// Command productloader seeds the catalogue.
//
// The load test browses product ids p1..pN, so those products have to exist
// before a run means anything: against an empty catalogue every browse is a
// 404 and the measurement is of the error path. This tool fills that gap.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/envx"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/product"
)

func main() {
	target := flag.String("url", envx.String("PRODUCT_SERVICE_URL", "http://localhost:8081"),
		"base URL of the product service")
	count := flag.Int("count", envx.Int("PRODUCT_COUNT", 1000),
		"how many products to create")
	start := flag.Int("start", envx.Int("PRODUCT_START", 1),
		"first product number")
	prefix := flag.String("prefix", envx.String("PRODUCT_ID_PREFIX", "p"),
		"product id prefix; ids are <prefix><n>")
	workers := flag.Int("workers", envx.Int("LOADER_WORKERS", 16),
		"concurrent upload workers")
	attempts := flag.Int("attempts", envx.Int("LOADER_ATTEMPTS", 3),
		"attempts per product before giving up")
	flag.Parse()

	obs.InitLogging("product-loader")

	if *count <= 0 {
		slog.Error("count must be positive", "count", *count)
		os.Exit(1)
	}
	if *workers <= 0 {
		*workers = 1
	}

	client := product.NewClient(*target, 2*time.Second, 30*time.Second)

	ctx, stop := httpx.SignalContext()
	defer stop()

	slog.Info("seeding catalogue", "url", *target, "count", *count, "workers", *workers)
	began := time.Now()

	// One generator feeding a channel of work, N uploaders draining it. The
	// channel is the backpressure: generation cannot outrun the uploads.
	work := make(chan product.Product, *workers*4)
	go func() {
		defer close(work)
		for i := 0; i < *count; i++ {
			select {
			case <-ctx.Done():
				return
			case work <- generate(fmt.Sprintf("%s%d", *prefix, *start+i)):
			}
		}
	}()

	var succeeded, failed atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				if err := upload(ctx, client, p, *attempts); err != nil {
					failed.Add(1)
					slog.Warn("could not create product", "productId", p.ProductID, "err", err)
					continue
				}
				if n := succeeded.Add(1); n%500 == 0 {
					slog.Info("progress", "created", n, "of", *count)
				}
			}
		}()
	}
	wg.Wait()

	elapsed := time.Since(began)
	slog.Info("done",
		"created", succeeded.Load(),
		"failed", failed.Load(),
		"elapsed", elapsed.Round(time.Millisecond),
		"rate_per_sec", int(float64(succeeded.Load())/elapsed.Seconds()),
	)

	if failed.Load() > 0 {
		os.Exit(1)
	}
}

func upload(ctx context.Context, client *product.Client, p product.Product, attempts int) error {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := client.Put(ctx, p)
		if err == nil {
			return nil
		}
		lastErr = err
		// The catalogue's write path is a synchronous quorum write, so a
		// failure here is usually a replica still coming up. Backing off gives
		// the cluster time to finish forming instead of hammering it.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 250 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("unknown failure")
	}
	return lastErr
}

// generate builds a plausible product. Weights are log-normal so most items
// are light and a few are heavy, which is what a real catalogue looks like and
// what makes cart weight totals worth computing.
func generate(productID string) product.Product {
	weight := clamp(math.Exp(rand.NormFloat64()*0.9-0.4), 0.05, 60)
	price := clamp(math.Exp(rand.NormFloat64()*0.8+2.9), 0.99, 5000)

	return product.Product{
		ProductID: productID,
		Name:      fmt.Sprintf("%s %s", adjectives[rand.IntN(len(adjectives))], nouns[rand.IntN(len(nouns))]),
		Weight:    round2(weight),
		Price:     round2(price),
	}
}

var (
	adjectives = []string{
		"Compact", "Rugged", "Wireless", "Insulated", "Adjustable", "Portable",
		"Stainless", "Ceramic", "Refurbished", "Heavy-Duty", "Ultralight", "Modular",
	}
	nouns = []string{
		"Desk Lamp", "Water Bottle", "Keyboard", "Backpack", "Monitor Stand",
		"Cable Kit", "Frying Pan", "Headphones", "Tool Set", "Office Chair",
		"Space Heater", "Bookshelf",
	}
)

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
