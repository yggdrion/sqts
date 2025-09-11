package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
)

// Server represents a server configuration
type Server struct {
	Name string `json:"Name"`
	URL  string `json:"Url"`
}

// BattleMetricsResponse represents the API response structure (simplified)
type BattleMetricsResponse struct {
	Data struct {
		Attributes struct {
			Name    string `json:"name"`
			Players int    `json:"players"`
			Details struct {
				Map           string `json:"map"`
				GameMode      string `json:"gameMode"`
				SquadPlayTime int    `json:"squad_playTime"`
				SquadTeamOne  string `json:"squad_teamOne"`
				SquadTeamTwo  string `json:"squad_teamTwo"`
			} `json:"details"`
		} `json:"attributes"`
	} `json:"data"`
}

// Prometheus metrics
var (
	// Main metrics with stable labels only
	fridaSquadPlayerCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sqts_squad_player_count",
			Help: "Number of players on the squad server",
		},
		[]string{"server_short_name"},
	)

	fridaSquadPlayTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sqts_squad_play_time_seconds",
			Help: "Current round play time in seconds",
		},
		[]string{"server_short_name"},
	)

	// Info metric for server metadata (labels change infrequently)
	fridaSquadServerInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sqts_squad_server_info",
			Help: "Server information and metadata",
		},
		[]string{
			"server_short_name",
			"server_full_name",
			"map_name",
			"game_mode",
			"team_one",
			"team_two",
		},
	)

	scrapeErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "squad_server_scrape_errors_total",
			Help: "Total number of scrape errors",
		},
		[]string{"server_name"},
	)
)

// MetricsCollector handles collecting metrics from BattleMetrics API
type MetricsCollector struct {
	servers     []Server
	httpClient  *http.Client
	rateLimiter *rate.Limiter
}

// NewMetricsCollector creates a new metrics collector with rate limiting
func NewMetricsCollector(servers []Server) *MetricsCollector {
	// BattleMetrics limits: 60 requests/minute, 15 requests/second
	// We'll use 1 request per second average with burst of 10 to be safe
	rateLimiter := rate.NewLimiter(rate.Every(time.Second), 10)

	return &MetricsCollector{
		servers:     servers,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		rateLimiter: rateLimiter,
	}
}

// fetchServerData fetches data from BattleMetrics API for a single server
func (mc *MetricsCollector) fetchServerData(server Server) error {
	// Wait for rate limiter permission
	ctx := context.Background()
	if err := mc.rateLimiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limiter error for server %s: %w", server.Name, err)
	}

	resp, err := mc.httpClient.Get(server.URL)
	if err != nil {
		scrapeErrors.WithLabelValues(server.Name).Inc()
		return fmt.Errorf("failed to fetch data for server %s: %w", server.Name, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Failed to close response body for server %s: %v", server.Name, closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		scrapeErrors.WithLabelValues(server.Name).Inc()
		return fmt.Errorf("unexpected status code %d for server %s", resp.StatusCode, server.Name)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		scrapeErrors.WithLabelValues(server.Name).Inc()
		return fmt.Errorf("failed to read response body for server %s: %w", server.Name, err)
	}

	var bmResp BattleMetricsResponse
	if err := json.Unmarshal(body, &bmResp); err != nil {
		scrapeErrors.WithLabelValues(server.Name).Inc()
		return fmt.Errorf("failed to unmarshal response for server %s: %w", server.Name, err)
	}

	// Update metrics
	mc.updateMetrics(server.Name, bmResp)
	return nil
}

// updateMetrics updates Prometheus metrics with server data
func (mc *MetricsCollector) updateMetrics(serverName string, resp BattleMetricsResponse) {
	attrs := resp.Data.Attributes

	// Update main metrics with stable labels only
	fridaSquadPlayerCount.WithLabelValues(serverName).Set(float64(attrs.Players))
	fridaSquadPlayTime.WithLabelValues(serverName).Set(float64(attrs.Details.SquadPlayTime))

	// Update info metric with current metadata (value is always 1)
	fridaSquadServerInfo.WithLabelValues(
		serverName,                 // server_short_name
		attrs.Name,                 // server_full_name
		attrs.Details.Map,          // map_name
		attrs.Details.GameMode,     // game_mode
		attrs.Details.SquadTeamOne, // team_one
		attrs.Details.SquadTeamTwo, // team_two
	).Set(1)
}

// collectMetrics fetches data for all servers with rate limiting
func (mc *MetricsCollector) collectMetrics() {
	log.Printf("Starting metrics collection for %d servers", len(mc.servers))

	// Process servers sequentially to respect rate limits
	for _, server := range mc.servers {
		if err := mc.fetchServerData(server); err != nil {
			log.Printf("Error fetching data for server %s: %v", server.Name, err)
		}
	}

	log.Printf("Completed metrics collection")
}

// startMetricsCollection starts a goroutine that periodically collects metrics
func (mc *MetricsCollector) startMetricsCollection(interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		// Collect metrics immediately on startup
		mc.collectMetrics()

		for range ticker.C {
			mc.collectMetrics()
		}
	}()
}

func loadServers(filename string) ([]Server, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read servers file: %w", err)
	}

	var servers []Server
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil, fmt.Errorf("failed to decode servers JSON: %w", err)
	}

	return servers, nil
}

func main() {
	// Load server configurations
	servers, err := loadServers("servers.json")
	if err != nil {
		log.Fatalf("Failed to load servers: %v", err)
	}

	log.Printf("Loaded %d servers", len(servers))
	log.Printf("Rate limiting: 1 request/second, collection will take ~%d seconds", len(servers))

	// Register Prometheus metrics
	prometheus.MustRegister(fridaSquadPlayerCount, fridaSquadPlayTime, fridaSquadServerInfo, scrapeErrors)

	// Create metrics collector
	collector := NewMetricsCollector(servers)

	// Start collecting metrics every 60 seconds (gives enough time for all 12 servers)
	collector.startMetricsCollection(60 * time.Second)

	// Setup Chi router
	r := chi.NewRouter()
	r.Use(middleware.Logger, middleware.Recoverer, middleware.Heartbeat("/health"))
	r.Handle("/metrics", promhttp.Handler())
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"service":   "Squad Server Metrics",
			"servers":   len(servers),
			"endpoints": []string{"/metrics", "/health"},
		}); err != nil {
			log.Printf("Failed to encode JSON response: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	})

	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	log.Printf("Starting server on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}
