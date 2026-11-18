// Command loadgen sends realistic parking traffic through the public edge and
// reports exactly what clients experienced, broken down by release.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const maxConcurrent = 128

// Report summarises one load run.
type Report struct {
	Total           int                       `json:"total"`
	TransportErrors int                       `json:"transport_errors"`
	GatewayErrors   int                       `json:"gateway_errors"`
	AppErrors       int                       `json:"app_errors"`
	ByStatus        map[string]int            `json:"by_status"`
	ByVersion       map[string]map[string]int `json:"by_version"`
	LastFailureAt   *time.Time                `json:"last_failure_at,omitempty"`
}

type collector struct {
	mu     sync.Mutex
	report Report
}

func (c *collector) failed() {
	now := time.Now().UTC()
	c.report.LastFailureAt = &now
}

func (c *collector) transportError() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.report.Total++
	c.report.TransportErrors++
	c.failed()
}

func (c *collector) response(status int, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	code := strconv.Itoa(status)
	c.report.Total++
	c.report.ByStatus[code]++
	if version == "" {
		version = "edge"
	}
	if c.report.ByVersion[version] == nil {
		c.report.ByVersion[version] = map[string]int{}
	}
	c.report.ByVersion[version][code]++
	switch {
	case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
		c.report.GatewayErrors++
		c.failed()
	case status >= http.StatusInternalServerError:
		c.report.AppErrors++
		c.failed()
	}
}

type client struct {
	base    string
	http    *http.Client
	metrics *collector
}

func (c *client) call(ctx context.Context, method, path string, body any) (int, []byte) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, nil
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.metrics.transportError()
		}
		return 0, nil
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	c.metrics.response(resp.StatusCode, resp.Header.Get("X-App-Version"))
	return resp.StatusCode, payload
}

func (c *client) scenario(ctx context.Context) {
	switch roll := rand.IntN(10); {
	case roll < 5:
		c.call(ctx, http.MethodGet, "/api/v1/spots?free=true&type=electric", nil)
	case roll < 8:
		c.call(ctx, http.MethodGet, "/api/v1/tariffs", nil)
	default:
		c.parkAndLeave(ctx)
	}
}

func (c *client) parkAndLeave(ctx context.Context) {
	plate := fmt.Sprintf("LOAD%06d", rand.IntN(1_000_000))
	status, payload := c.call(ctx, http.MethodPost, "/api/v1/reservations", map[string]any{
		"spot_id":       1 + rand.IntN(400),
		"license_plate": plate,
	})
	if status != http.StatusCreated {
		return
	}
	var created struct {
		ID int `json:"id"`
	}
	if json.Unmarshal(payload, &created) == nil && created.ID > 0 {
		c.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/reservations/%d/release", created.ID), nil)
	}
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8088", "public base URL")
	rps := flag.Int("rps", 40, "request scenarios per second")
	duration := flag.Duration("duration", 0, "how long to run; 0 means until interrupted")
	output := flag.String("out", "", "write the JSON report to this file instead of stdout")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	metrics := &collector{report: Report{ByStatus: map[string]int{}, ByVersion: map[string]map[string]int{}}}
	c := &client{
		base:    *base,
		metrics: metrics,
		http: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{MaxIdleConns: maxConcurrent, MaxIdleConnsPerHost: maxConcurrent},
		},
	}

	var workers sync.WaitGroup
	slots := make(chan struct{}, maxConcurrent)
	ticker := time.NewTicker(time.Second / time.Duration(*rps))
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			select {
			case slots <- struct{}{}:
			default:
				continue
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { <-slots }()
				// In-flight requests may finish after the run is told to stop.
				c.scenario(context.WithoutCancel(ctx))
			}()
		}
	}
	workers.Wait()

	raw, err := json.MarshalIndent(metrics.report, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *output == "" {
		fmt.Println(string(raw))
		return
	}
	if err := os.WriteFile(*output, raw, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
