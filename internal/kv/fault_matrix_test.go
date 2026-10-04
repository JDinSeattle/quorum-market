package kv

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// An unavailable replica returns an explicit 503 at its real HTTP boundary.
// Product R=1 requires its serving coordinator to be unavailable to miss R;
// merely losing a follower cannot make that local read quorum insufficient.
func TestFixedEighteenCellQuorumFaultMatrix(t *testing.T) {
	profiles := []struct {
		name    string
		n, w, r int
		mode    Mode
	}{
		{"product", 5, 5, 1, ModeLeaderFollower}, {"cart", 5, 3, 3, ModeLeaderless}, {"core", 3, 2, 2, ModeLeaderless},
	}
	for _, profile := range profiles {
		for _, insufficient := range []bool{false, true} {
			for _, operation := range []string{"write", "read", "scan"} {
				label := "reachable"
				if insufficient {
					label = "insufficient"
				}
				t.Run(profile.name+"/"+label+"/"+operation, func(t *testing.T) {
					handlers := make([]http.Handler, profile.n)
					servers := make([]*httptest.Server, profile.n)
					disabled := make([]atomic.Bool, profile.n)
					for i := range servers {
						index := i
						servers[i] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if disabled[index].Load() {
								http.Error(w, "injected unavailable replica", http.StatusServiceUnavailable)
								return
							}
							handlers[index].ServeHTTP(w, r)
						}))
					}
					for i := range servers {
						cfg := Config{NodeID: fmt.Sprintf("matrix-%d", i), Mode: profile.mode, Role: "leader", WriteQuorum: profile.w, ReadQuorum: profile.r, RPCTimeout: time.Second}
						if cfg.Mode == ModeLeaderFollower && i != 0 {
							cfg.Role = "follower"
						}
						for j, server := range servers {
							if i != j {
								cfg.Peers = append(cfg.Peers, "http://"+server.Listener.Addr().String())
							}
						}
						store := NewStore(cfg.NodeID)
						store.Apply(Entry{Key: "matrix:key", Value: "fixture", Version: 1, Origin: "fixture"})
						service := NewService(cfg, store, NewReplicator(time.Second))
						handlers[i] = NewServer(service, NewTxnManager(time.Minute), busywait.Config{}, false).Routes()
						servers[i].Start()
						t.Cleanup(servers[i].Close)
					}
					if insufficient {
						quorum := profile.r
						if operation == "write" {
							quorum = profile.w
						}
						for i := range disabled {
							disabled[i].Store(i >= quorum-1)
						}
					}
					method, path, body := "GET", "/kv?key=matrix:key", ""
					if operation == "scan" {
						path = "/kv/scan?prefix=matrix:"
					}
					if operation == "write" {
						method = "PUT"
						path = "/kv"
						body = `{"key":"matrix:new","value":"value"}`
					}
					req, e := http.NewRequest(method, servers[0].URL+path, strings.NewReader(body))
					if e != nil {
						t.Fatal(e)
					}
					req.Header.Set("Content-Type", "application/json")
					response, e := (&http.Client{Timeout: 3 * time.Second}).Do(req)
					if e != nil {
						t.Fatal(e)
					}
					defer response.Body.Close()
					io.Copy(io.Discard, response.Body)
					if insufficient {
						if response.StatusCode != 503 {
							t.Fatalf("insufficient quorum returned %d", response.StatusCode)
						}
					} else if response.StatusCode < 200 || response.StatusCode >= 300 {
						t.Fatalf("reachable returned %d", response.StatusCode)
					}
				})
			}
		}
	}
}

// A successful first peer must not let a failed final peer count as a vote.
func TestSequentialWriteRejectsOneMissingAcknowledgement(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	cfg := Config{NodeID: "sequential", Mode: ModeLeaderless, WriteQuorum: 3, ReadQuorum: 1, RPCTimeout: time.Second, Sequential: true, Peers: []string{good.URL, bad.URL}}
	service := NewService(cfg, NewStore(cfg.NodeID), NewReplicator(time.Second))
	if _, err := service.Write(context.Background(), "partial", "value", 0); !httpx.IsStatus(err, http.StatusServiceUnavailable) {
		t.Fatalf("missing vote returned %v", err)
	}
}
