package main

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const cacheExpiry = 24 * time.Hour * 7

var rdb *redis.Client

func main() {
	host := os.Getenv("HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	redisAddr := os.Getenv("REDIS_ADDR")

	addr := host + ":" + port

	rdb = redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})
	if _, err := rdb.Ping(context.Background()).Result(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	http.HandleFunc("/", handler)

	log.Printf("Listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

var mu sync.Mutex
var lastReq time.Time

func handler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log.Printf("Request from %s for %s?%s", r.RemoteAddr, r.URL.Path, r.URL.RawQuery)

	apiKey := r.Header.Get("X-API-Key")
	if apiKey == "" {
		http.Error(w, "missing X-API-Key", http.StatusUnauthorized)
		return
	}

	cacheKey := cacheKey(apiKey, r.URL.Path, r.URL.Query())

	// Check the cache

	cached, err := rdb.Get(ctx, cacheKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		log.Printf("Failed to lookup %s in cache: %v", cacheKey, err)
		http.Error(w, "cache lookup failed", http.StatusInternalServerError)
		return
	}
	if cached != "" {
		ct, body, err := parseCached(cached)
		if err != nil {
			log.Printf("Failed to parse cached response: %v", err)
			http.Error(w, "failed to parse cached response", http.StatusInternalServerError)
			return
		}
		log.Printf("Cache hit for %s", cacheKey)
		w.Header().Set("Content-Type", ct)
		_, _ = w.Write([]byte(body))
		return
	}

	// Not in cache, make the request

	log.Printf("Cache miss for %s", cacheKey)

	mu.Lock()
	if time.Since(lastReq) < time.Second {
		mu.Unlock()
		log.Println("Rejecting request, too fast")
		http.Error(w, "too fast", http.StatusTooManyRequests)
		return
	}
	lastReq = time.Now()
	mu.Unlock()

	query := r.URL.Query()
	query.Set("api_key", apiKey)

	client := http.Client{
		Timeout: 10 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.flickr.com"+r.URL.Path+"?"+query.Encode(), nil)
	if err != nil {
		log.Printf("Failed to create request: %v", err)
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Failed to make request: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("HTTP status %d", resp.StatusCode)
		http.Error(w, resp.Status, resp.StatusCode)
		return
	}

	ct := resp.Header.Get("Content-Type")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read response body: %v", err)
		http.Error(w, "failed to read response body", http.StatusInternalServerError)
		return
	}

	// Cache the response

	cached = serializeCached(ct, string(body))
	err = rdb.Set(ctx, cacheKey, cached, cacheExpiry).Err()
	if err != nil {
		log.Printf("Failed to cache response: %v", err)
		http.Error(w, "failed to cache response", http.StatusInternalServerError)
		return
	}

	// Return the response

	log.Println("Received response from Flickr, returning to client")

	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(body)
}

func cacheKey(apiKey string, path string, query url.Values) string {
	hasher := sha256.New()
	hasher.Write([]byte(apiKey))
	hasher.Write([]byte(path))
	hasher.Write([]byte(query.Encode()))
	sum := hasher.Sum(nil)
	return "flickr-api-proxy:cache:" + base32.StdEncoding.EncodeToString(sum)
}

func parseCached(resp string) (string, string, error) {
	lenS := resp[:3]
	ctLen, err := strconv.ParseInt(lenS, 10, 64)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse content-type length: %v", err)
	}
	ct := resp[3 : 3+ctLen]
	body := resp[3+ctLen:]
	return ct, body, nil
}

func serializeCached(ct string, body string) string {
	return fmt.Sprintf("%03d%s%s", len(ct), ct, body)
}
