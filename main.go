package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/sync/singleflight"
)

const (
	searchAPIURL = "https://tidal-api.binimum.org/search/"
	trackAPIURL  = "https://tidal-api.binimum.org/track/"
	cacheTTL     = 5 * time.Minute
	httpTimeout  = 15 * time.Second
	serverPort   = ":8080"
)

type TrackItem struct {
	ID           int    `json:"id"`
	AudioQuality string `json:"audioQuality"`
}

type SearchResponse struct {
	Items []TrackItem `json:"items"`
}

type TrackURLItem struct {
	OriginalTrackURL string `json:"OriginalTrackUrl"`
}

type FinalResponse struct {
	URL string `json:"url"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type cacheItem struct {
	value     string
	expiresAt time.Time
}

type Cache struct {
	mu    sync.RWMutex
	items map[string]cacheItem
}

func NewCache() *Cache {
	return &Cache{
		items: make(map[string]cacheItem),
	}
}

func (c *Cache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, found := c.items[key]
	if !found {
		return "", false
	}
	if time.Now().After(item.expiresAt) {
		return "", false
	}
	return item.value, true
}

func (c *Cache) Set(key string, value string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = cacheItem{
		value:     value,
		expiresAt: time.Now().Add(ttl),
	}
}

type App struct {
	logger *slog.Logger
	client *http.Client
	cache  *Cache
	sf     *singleflight.Group
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	httpClient := &http.Client{
		Timeout: httpTimeout,
	}
	app := &App{
		logger: logger,
		client: httpClient,
		cache:  NewCache(),
		sf:     &singleflight.Group{},
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/search/{query}", app.searchHandler)

	server := &http.Server{
		Addr:    serverPort,
		Handler: r,
	}

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("Server starting", "port", serverPort)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Server failed to start", "error", err)
			os.Exit(1)
		}
	}()

	<-stopChan
	logger.Info("Shutting down server gracefully...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("Graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("Server stopped")
}

func (app *App) searchHandler(w http.ResponseWriter, r *http.Request) {
	query := chi.URLParam(r, "query")
	if query == "" {
		app.jsonError(w, "Search query is required", http.StatusBadRequest)
		return
	}

	log := app.logger.With(slog.String("query", query), slog.String("request_id", middleware.GetReqID(r.Context())))
	log.Info("Received search request")

	if cachedURL, found := app.cache.Get(query); found {
		log.Info("Cache hit")
		app.jsonResponse(w, FinalResponse{URL: cachedURL}, http.StatusOK)
		return
	}
	log.Info("Cache miss")

	v, err, _ := app.sf.Do(query, func() (interface{}, error) {
		tracks, err := app.searchTracks(r.Context(), query)
		if err != nil {
			return nil, err
		}

		finalURL, err := app.findFirstValidTrackURL(r.Context(), tracks, log)
		if err != nil {
			return nil, err
		}

		app.cache.Set(query, finalURL, cacheTTL)
		log.Info("Result cached", "ttl", cacheTTL.String())
		return finalURL, nil
	})

	if err != nil {
		var statusErr *statusError
		if errors.As(err, &statusErr) {
			log.Warn("Failed to find track URL", "error", statusErr.Error())
			app.jsonError(w, statusErr.Error(), statusErr.Code)
		} else {
			log.Error("Internal error during singleflight execution", "error", err)
			app.jsonError(w, "Internal server error", http.StatusInternalServerError)
		}
		return
	}

	finalURL := v.(string)
	app.jsonResponse(w, FinalResponse{URL: finalURL}, http.StatusOK)
}

func (app *App) findFirstValidTrackURL(ctx context.Context, tracks []TrackItem, log *slog.Logger) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	urlChan := make(chan string, 1)
	var wg sync.WaitGroup

	for i, track := range tracks {
		wg.Add(1)
		go func(track TrackItem, attempt int) {
			defer wg.Done()
			trackLog := log.With(slog.Int("track_id", track.ID), slog.Int("attempt", attempt))

			select {
			case <-ctx.Done():
				return
			default:
			}

			finalURL, err := app.getTrackURL(ctx, track.ID, track.AudioQuality)
			if err != nil {
				var statusErr *statusError
				if errors.As(err, &statusErr) && statusErr.Code == http.StatusNotFound {
					trackLog.Warn("Track details not found (404), this goroutine will exit")
				} else {
					trackLog.Error("Unexpected error getting track URL", "error", err)
				}
				return
			}

			select {
			case urlChan <- finalURL:
			case <-ctx.Done():
			}
		}(track, i+1)
	}

	go func() {
		wg.Wait()
		close(urlChan)
	}()

	select {
	case <-ctx.Done():
		return "", &statusError{http.StatusNotFound, errors.New("operation cancelled before a result was found")}
	case finalURL, ok := <-urlChan:
		if ok {
			cancel()
			return finalURL, nil
		}
		log.Warn("Processed all search results but found no valid track URL")
		return "", &statusError{http.StatusNotFound, errors.New("track not found")}
	}
}

func (app *App) searchTracks(ctx context.Context, query string) ([]TrackItem, error) {
	encodedQuery := url.QueryEscape(query)
	reqURL := fmt.Sprintf("%s?s=%s", searchAPIURL, encodedQuery)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create search request: %w", err)
	}

	resp, err := app.client.Do(req)
	if err != nil {
		return nil, &statusError{http.StatusBadGateway, fmt.Errorf("search API request failed: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{http.StatusBadGateway, fmt.Errorf("search API returned non-200 status: %d", resp.StatusCode)}
	}

	var searchResp SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, fmt.Errorf("failed to decode search response: %w", err)
	}

	if len(searchResp.Items) == 0 {
		return nil, &statusError{http.StatusNotFound, errors.New("track not found")}
	}

	return searchResp.Items, nil
}

func (app *App) getTrackURL(ctx context.Context, id int, quality string) (string, error) {
	reqURL := fmt.Sprintf("%s?id=%d&quality=%s", trackAPIURL, id, quality)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create track URL request: %w", err)
	}

	resp, err := app.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		return "", &statusError{http.StatusBadGateway, fmt.Errorf("track URL API request failed: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", &statusError{http.StatusNotFound, fmt.Errorf("upstream API returned 404 for track ID %d", id)}
	}

	if resp.StatusCode != http.StatusOK {
		return "", &statusError{http.StatusBadGateway, fmt.Errorf("track URL API returned non-200 status: %d", resp.StatusCode)}
	}

	var trackInfo []TrackURLItem
	if err := json.NewDecoder(resp.Body).Decode(&trackInfo); err != nil {
		return "", fmt.Errorf("failed to decode track URL response: %w", err)
	}

	for _, item := range trackInfo {
		if item.OriginalTrackURL != "" {
			return item.OriginalTrackURL, nil
		}
	}

	return "", &statusError{http.StatusBadGateway, errors.New("could not find OriginalTrackUrl in upstream API response")}
}

type statusError struct {
	Code int
	Err  error
}

func (se *statusError) Error() string {
	return se.Err.Error()
}

func (app *App) jsonError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(ErrorResponse{Error: message})
}

func (app *App) jsonResponse(w http.ResponseWriter, data interface{}, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}
