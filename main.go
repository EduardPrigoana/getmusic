package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultPort     = "8080"
	defaultQuality  = 27
	requestTimeout  = 10 * time.Second
	shutdownTimeout = 5 * time.Second
	searchAPIPath   = "/api/get-music"
	downloadAPIPath = "/api/download-music"
)

var (
	qobuzdlInstance string
	httpClient      *http.Client
	validQualities  = map[int]struct{}{5: {}, 6: {}, 7: {}, 27: {}}
	featRegex       = regexp.MustCompile(`(?i)\b(feat|ft)\b.*`)
)

type DownloadResponse struct {
	Data struct {
		URL string `json:"url"`
	} `json:"data"`
}

type SuccessResponse struct {
	URL string `json:"url"`
}

type ErrorDetail struct {
	Code        int    `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type apiError struct {
	statusCode int
	message    string
}

func (e *apiError) Error() string {
	return e.message
}

func init() {
	qobuzdlInstance = os.Getenv("QOBUZDL_INSTANCE")
	if qobuzdlInstance == "" {
		qobuzdlInstance = "https://qobuz.squid.wtf"
	}
	qobuzdlInstance = strings.TrimRight(qobuzdlInstance, "/")

	httpClient = &http.Client{
		Timeout: requestTimeout,
	}
}

func findFirstIDWithISRC(data interface{}) (string, bool) {
	switch val := data.(type) {
	case map[string]interface{}:
		idVal, idOk := val["id"]
		isrcVal, isrcOk := val["isrc"]

		if idOk && isrcOk {
			if isrcStr, ok := isrcVal.(string); ok && strings.TrimSpace(isrcStr) != "" {
				if idFloat, ok := idVal.(float64); ok {
					return fmt.Sprintf("%.0f", idFloat), true
				}
				if idStr, ok := idVal.(string); ok {
					return idStr, true
				}
			}
		}

		for _, v := range val {
			if id, found := findFirstIDWithISRC(v); found {
				return id, true
			}
		}

	case []interface{}:
		for _, item := range val {
			if id, found := findFirstIDWithISRC(item); found {
				return id, true
			}
		}
	}

	return "", false
}

func performSearch(ctx context.Context, query string, quality int) (string, *apiError) {
	if _, ok := validQualities[quality]; !ok {
		return "", &apiError{http.StatusBadRequest, fmt.Sprintf("Invalid quality value. Must be one of %v.", getValidQualitiesList())}
	}

	strippedQuery := strings.TrimSpace(featRegex.Split(query, 2)[0])
	if strippedQuery == "" {
		return "", &apiError{http.StatusBadRequest, "Search query is empty after cleaning."}
	}

	searchURL, _ := url.Parse(qobuzdlInstance + searchAPIPath)
	q := searchURL.Query()
	q.Set("q", strippedQuery)
	q.Set("offset", "0")
	searchURL.RawQuery = q.Encode()

	var searchData interface{}
	req, _ := http.NewRequestWithContext(ctx, "GET", searchURL.String(), nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("Error contacting search service: %v", err)
		return "", &apiError{http.StatusServiceUnavailable, "The external search service is unavailable."}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Search service returned non-200 status: %d", resp.StatusCode)
		return "", &apiError{http.StatusServiceUnavailable, "The external search service returned an error."}
	}

	if err := json.NewDecoder(resp.Body).Decode(&searchData); err != nil {
		log.Printf("Error decoding search service response: %v", err)
		return "", &apiError{http.StatusInternalServerError, "Received an invalid response from the search service."}
	}

	trackID, found := findFirstIDWithISRC(searchData)
	if !found {
		return "", &apiError{http.StatusNotFound, "No track with a valid ID and ISRC was found."}
	}

	downloadURL, _ := url.Parse(qobuzdlInstance + downloadAPIPath)
	q = downloadURL.Query()
	q.Set("track_id", trackID)
	q.Set("quality", strconv.Itoa(quality))
	downloadURL.RawQuery = q.Encode()

	var downloadData DownloadResponse
	req, _ = http.NewRequestWithContext(ctx, "GET", downloadURL.String(), nil)
	resp, err = httpClient.Do(req)
	if err != nil {
		log.Printf("Error contacting download service: %v", err)
		return "", &apiError{http.StatusServiceUnavailable, "The external download service is unavailable."}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Download service returned non-200 status: %d", resp.StatusCode)
		return "", &apiError{http.StatusServiceUnavailable, "The external download service returned an error."}
	}

	if err := json.NewDecoder(resp.Body).Decode(&downloadData); err != nil {
		log.Printf("Error decoding download service response: %v", err)
		return "", &apiError{http.StatusInternalServerError, "Received an invalid response from the download service."}
	}

	if downloadData.Data.URL == "" {
		return "", &apiError{http.StatusInternalServerError, "Download URL not found in the final API response."}
	}

	return downloadData.Data.URL, nil
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/search/")
	path = strings.Trim(path, "/")

	if path == "" {
		writeJSONError(w, http.StatusBadRequest, "Search query is missing.")
		return
	}

	query := path
	quality := defaultQuality

	if qualityIndex := strings.LastIndex(path, "/quality/"); qualityIndex != -1 {
		potentialQualityStr := path[qualityIndex+len("/quality/"):]
		if parsedQuality, err := strconv.Atoi(potentialQualityStr); err == nil {
			query = path[:qualityIndex]
			quality = parsedQuality
		}
	}

	unescapedQuery, err := url.PathUnescape(query)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid URL-encoded query.")
		return
	}
	if unescapedQuery == "" {
		writeJSONError(w, http.StatusBadRequest, "Search query is empty.")
		return
	}

	downloadURL, apiErr := performSearch(r.Context(), unescapedQuery, quality)
	if apiErr != nil {
		writeJSONError(w, apiErr.statusCode, apiErr.message)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(SuccessResponse{URL: downloadURL}); err != nil {
		log.Printf("Error encoding success response: %v", err)
	}
}

func writeJSONError(w http.ResponseWriter, code int, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(ErrorResponse{
		Error: ErrorDetail{
			Code:        code,
			Name:        http.StatusText(code),
			Description: description,
		},
	})
}

func getValidQualitiesList() []int {
	keys := make([]int, 0, len(validQualities))
	for k := range validQualities {
		keys = append(keys, k)
	}
	return keys
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/search/", searchHandler)

	server := &http.Server{
		Addr:    ":" + port,
		Handler: corsMiddleware(mux),
	}

	go func() {
		log.Printf("Server starting on port %s", port)
		log.Printf("Using QobuzDL instance: %s", qobuzdlInstance)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Could not listen on %s: %v\n", port, err)
		}
	}()

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, syscall.SIGINT, syscall.SIGTERM)
	<-stopChan
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown failed: %+v", err)
	}
	log.Println("Server gracefully stopped")
}
