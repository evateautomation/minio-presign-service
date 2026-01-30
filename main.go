package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

type PresignRequest struct {
	Bucket  string `json:"bucket"`
	Folder  string `json:"folder"` // optional
	Key     string `json:"key"`    // required (file name or object key)
	Days    int    `json:"days"`
	Hours   int    `json:"hours"`
	Minutes int    `json:"minutes"`
}

type PresignResponse struct {
	URL       string `json:"url"`
	Object    string `json:"object"`
	Bucket    string `json:"bucket"`
	ExpiresIn string `json:"expires_in"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func main() {
	port := getenv("PORT", "8080")

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/presign", presignHandler)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           withLogging(withAuth(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("minio-presign-service listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func presignHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}

	// Read raw body once (helps avoid edge cases + allows future debugging)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "unable to read request body"})
		return
	}
	bodyStr := string(bodyBytes)
	// Restore body for JSON decode
	r.Body = io.NopCloser(strings.NewReader(bodyStr))

	var req PresignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid JSON body"})
		return
	}

	req.Bucket = strings.TrimSpace(req.Bucket)
	req.Folder = strings.TrimSpace(req.Folder)
	req.Key = strings.TrimSpace(req.Key)

	if req.Bucket == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bucket is required"})
		return
	}
	if req.Key == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "key is required"})
		return
	}

	alias := getenv("MINIO_ALIAS", "myminio")

	// Build object path: folder + key (folder optional)
	objectPath := joinObjectPath(req.Folder, req.Key)
	target := fmt.Sprintf("%s/%s/%s", alias, req.Bucket, objectPath)

	expire := buildExpire(req.Days, req.Hours, req.Minutes)
	if expire == "" {
		// sensible default if caller sends 0,0,0
		expire = "15m"
	}

	// Run: mc share download --expire 10m myminio/bucket/path/to/file
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "mc", "share", "download", "--expire", expire, target)
	outBytes, cmdErr := cmd.CombinedOutput()
	out := string(outBytes)

	if ctx.Err() == context.DeadlineExceeded {
		writeJSON(w, http.StatusGatewayTimeout, ErrorResponse{Error: "mc command timed out"})
		return
	}
	if cmdErr != nil {
		// Return the mc error output (trimmed) to help debugging in n8n
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = cmdErr.Error()
		}
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: msg})
		return
	}

	// Extract the URL from the "Share:" line (this is the presigned URL).
	urlStr, err := extractShareURL(out)
	if err != nil {
		// include mc output to make debugging easy
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "could not parse Share URL. mc output: " + strings.TrimSpace(out),
		})
		return
	}

	// Optionally rewrite returned URL to a public hostname users can access.
	// Use this if mc outputs an internal hostname.
	// e.g. PUBLIC_MINIO_BASE_URL=https://minio2.evatefinance.com
	urlStr = rewritePublicBase(urlStr)

	writeJSON(w, http.StatusOK, PresignResponse{
		URL:       urlStr,
		Object:    objectPath,
		Bucket:    req.Bucket,
		ExpiresIn: expire,
	})
}

func withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Allow /health without auth
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		token := strings.TrimSpace(os.Getenv("API_TOKEN"))
		if token == "" {
			// If API_TOKEN not set, deny by default (safer)
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "API_TOKEN is not set on server"})
			return
		}

		// Support either:
		// 1) x-api-token: <token>
		// 2) Authorization: Bearer <token>
		h1 := strings.TrimSpace(r.Header.Get("x-api-token"))
		h2 := strings.TrimSpace(r.Header.Get("Authorization"))

		ok := false
		if h1 != "" && h1 == token {
			ok = true
		}
		if !ok && strings.HasPrefix(strings.ToLower(h2), "bearer ") {
			if strings.TrimSpace(h2[7:]) == token {
				ok = true
			}
		}

		if !ok {
			writeJSON(w, http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
			return
		}

		next.ServeHTTP(w, r)
	})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func buildExpire(days, hours, minutes int) string {
	if days < 0 || hours < 0 || minutes < 0 {
		return ""
	}
	if days == 0 && hours == 0 && minutes == 0 {
		return ""
	}

	// mc accepts composite durations like "2d3h15m"
	sb := strings.Builder{}
	if days > 0 {
		sb.WriteString(fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		sb.WriteString(fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		sb.WriteString(fmt.Sprintf("%dm", minutes))
	}
	return sb.String()
}

func joinObjectPath(folder, key string) string {
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "/")
		s = strings.TrimSuffix(s, "/")
		return s
	}
	f := clean(folder)
	k := clean(key)
	if f == "" {
		return k
	}
	return f + "/" + k
}

// extractShareURL returns the presigned URL printed on the "Share:" line by `mc share download`.
func extractShareURL(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Share:") {
			u := strings.TrimSpace(strings.TrimPrefix(line, "Share:"))
			if u == "" {
				return "", errors.New("Share line present but empty")
			}
			return u, nil
		}
	}
	return "", errors.New("could not find Share: line in mc output")
}

func rewritePublicBase(u string) string {
	publicBase := strings.TrimSpace(os.Getenv("PUBLIC_MINIO_BASE_URL"))
	if publicBase == "" {
		return u
	}

	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	pub, err := url.Parse(publicBase)
	if err != nil {
		return u
	}

	// Keep the path + query, swap only scheme/host.
	parsed.Scheme = pub.Scheme
	parsed.Host = pub.Host
	return parsed.String()
}

func getenv(k, def string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	return v
}
