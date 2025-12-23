// http.ingest (text mode)
// - Purpose: nhận HTTPrequest raw và in ra stdout dạng text: REQUEST_LINE + headers + blank line + body.
// - Safety: giới hạn body bằng MAX_BODY_BYTES; có thể redact Cookie/Authorization bằng REDACT_SENSITIVE.
package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"strings"
	"time"
	"unicode/utf8"
)

// logState: thống kê nhẹ để /logcheck kiểm tra "có đang nhận & log request không".
// (in-memory, không ghi disk; reset khi container restart)
type logState struct {
	startedAt time.Time
	reqTotal  uint64

	mu          sync.Mutex
	lastAt      time.Time
	lastMethod  string
	lastPath    string
	lastQuery   string
	lastCT      string
	lastCL      string
	lastRemote  string
	lastSource  string
}

// main: start HTTP server, nhận request, giới hạn body, lọc/redact headers, rồi in ra request text.
func main() {
	port := getenv("PORT", "9002")
	redactSensitive := strings.EqualFold(getenv("REDACT_SENSITIVE", "true"), "true")
	st := &logState{startedAt: time.Now().UTC()}

	allow := parseHeaderAllowlist(getenv("HEADER_ALLOWLIST",
		"host,user-agent,accept,accept-language,accept-encoding,content-type,content-length,referer,origin,"+
			"x-forwarded-for,x-real-ip,x-request-id,x-correlation-id,sec-fetch-site,sec-fetch-mode,sec-fetch-dest,"+
			"sec-ch-ua,sec-ch-ua-mobile,sec-ch-ua-platform,authorization,cookie"))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	// /favicon.ico: browser hay tự request khi mở /logcheck => trả 204 để khỏi nhiễu log.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// /logcheck: kiểm tra nhanh tình trạng "log pipeline" (có request nào vào chưa, request gần nhất là gì).
	mux.HandleFunc("/logcheck", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()

		total := atomic.LoadUint64(&st.reqTotal)
		uptime := now.Sub(st.startedAt)

		st.mu.Lock()
		lastAt := st.lastAt
		lastMethod := st.lastMethod
		lastPath := st.lastPath
		lastQuery := st.lastQuery
		lastCT := st.lastCT
		lastCL := st.lastCL
		lastRemote := st.lastRemote
		lastSource := st.lastSource
		st.mu.Unlock()

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "status=ok\n")
		_, _ = fmt.Fprintf(w, "started_at=%s\n", st.startedAt.Format(time.RFC3339))
		_, _ = fmt.Fprintf(w, "uptime_seconds=%d\n", int64(uptime.Seconds()))
		_, _ = fmt.Fprintf(w, "requests_total=%d\n", total)
		if !lastAt.IsZero() {
			_, _ = fmt.Fprintf(w, "last_request_at=%s\n", lastAt.Format(time.RFC3339Nano))
			_, _ = fmt.Fprintf(w, "last_request=%s %s?%s\n", lastMethod, lastPath, lastQuery)
			if lastCT != "" {
				_, _ = fmt.Fprintf(w, "last_content_type=%s\n", lastCT)
			}
			if lastCL != "" {
				_, _ = fmt.Fprintf(w, "last_content_length=%s\n", lastCL)
			}
			if lastSource != "" {
				_, _ = fmt.Fprintf(w, "last_source_ip=%s\n", lastSource)
			}
			if lastRemote != "" {
				_, _ = fmt.Fprintf(w, "last_remote_addr=%s\n", lastRemote)
			}
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()

		// Read full request body (no size limit) to preserve "raw request" for training.
		bodyBytes, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		bodyBytesStripped, bomStripped := stripUTF8BOM(bodyBytes)
		bodyRaw, bodyEncoding := bytesToText(bodyBytesStripped)

		headers := filterHeaders(r.Header, allow, redactSensitive)

		// Update /logcheck state
		atomic.AddUint64(&st.reqTotal, 1)
		st.mu.Lock()
		st.lastAt = time.Now().UTC()
		st.lastMethod = r.Method
		st.lastPath = r.URL.Path
		st.lastQuery = r.URL.RawQuery
		st.lastCT = headers["content-type"]
		st.lastCL = headers["content-length"]
		st.lastRemote = r.RemoteAddr
		st.lastSource = extractSourceIP(r)
		st.mu.Unlock()

		text := renderHTTPRequestText(r, headers, bodyRaw, bodyEncoding, bomStripped)
		_, _ = fmt.Fprintln(os.Stdout, text)
		// Separator between requests (like a log record boundary).
		_, _ = fmt.Fprintln(os.Stdout, "")

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("http.ingest listening on :%s\n", port)
	log.Fatal(srv.ListenAndServe())
}

// renderHTTPRequestText: dựng block "raw HTTP" dạng text (request line + headers + blank line + body).
func renderHTTPRequestText(r *http.Request, headers map[string]string, bodyRaw string, bodyEncoding string, bomStripped bool) string {
	var b strings.Builder

	// Request line
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	if r.URL.RawQuery != "" {
		path = path + "?" + r.URL.RawQuery
	}
	fmt.Fprintf(&b, "%s %s HTTP/1.1\n", r.Method, path)

	// Host
	host := r.Host
	if host == "" {
		host = headers["host"]
	}
	if host != "" {
		fmt.Fprintf(&b, "Host: %s\n", host)
	}

	// Prefer a stable, meaningful header set (similar to raw HTTP request),
	// while keeping it safe for blue-team pipelines.
	writeHeader := func(k string) {
		if v, ok := headers[k]; ok && v != "" {
			fmt.Fprintf(&b, "%s: %s\n", canonicalHeaderKey(k), v)
		}
	}

	writeHeader("content-type")
	// Keep original Content-Length if present (raw request style).
	writeHeader("content-length")

	// A few commonly useful headers (you can expand via HEADER_ALLOWLIST).
	writeHeader("user-agent")
	writeHeader("accept")
	writeHeader("accept-language")
	writeHeader("accept-encoding")
	writeHeader("referer")
	writeHeader("origin")
	writeHeader("x-forwarded-for")
	writeHeader("x-real-ip")
	writeHeader("x-request-id")
	writeHeader("x-correlation-id")
	// Sensitive ones are redacted by filterHeaders (if allowlisted)
	writeHeader("authorization")
	writeHeader("cookie")

	// Blank line separates headers from body
	b.WriteString("\n")

	// Body (raw text; binary becomes base64)
	if bodyRaw != "" {
		b.WriteString(bodyRaw)
	}

	_ = bodyEncoding
	_ = bomStripped
	return b.String()
}

// stripUTF8BOM: bỏ BOM UTF-8 (thường gặp trên Windows PowerShell) để body sạch.
func stripUTF8BOM(b []byte) ([]byte, bool) {
	// Some Windows tooling (e.g., Windows PowerShell `Set-Content -Encoding utf8`)
	// writes UTF-8 with BOM. Strip it so JSON parsing works and the raw text is
	// AI-friendly.
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:], true
	}
	return b, false
}

// getenv: lấy env var, nếu rỗng thì dùng default.
func getenv(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

// getenvInt: lấy env var kiểu int (invalid/<=0 => default).
// (removed) body size limiting: always read full body to preserve raw request for training.

// parseHeaderAllowlist: parse danh sách header cho phép (CSV) -> set lowercase.
func parseHeaderAllowlist(csv string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, p := range strings.Split(csv, ",") {
		k := strings.ToLower(strings.TrimSpace(p))
		if k == "" {
			continue
		}
		out[k] = struct{}{}
	}
	return out
}

// filterHeaders: lọc headers theo allowlist; có thể redact Cookie/Authorization.
func filterHeaders(h http.Header, allow map[string]struct{}, redactSensitive bool) map[string]string {
	out := make(map[string]string, 16)
	for k, vals := range h {
		lk := strings.ToLower(k)
		if _, ok := allow[lk]; !ok {
			continue
		}
		v := strings.Join(vals, ", ")
		if redactSensitive && (lk == "authorization" || lk == "cookie" || lk == "set-cookie") {
			out[lk] = "[REDACTED]"
			continue
		}
		out[lk] = safeHeaderValue(v)
	}
	return out
}

// safeHeaderValue: trim + giới hạn độ dài header để log không quá to.
func safeHeaderValue(v string) string {
	v = strings.TrimSpace(v)
	// Avoid huge headers blowing up logs / LLM context.
	const max = 2048
	if len(v) > max {
		return v[:max] + "...(truncated)"
	}
	return v
}

// canonicalHeaderKey: đổi header key về dạng "Title-Case" để nhìn giống raw HTTP.
func canonicalHeaderKey(k string) string {
	// Keep output looking like standard HTTP request headers.
	parts := strings.Split(strings.ToLower(k), "-")
	for i := range parts {
		if parts[i] == "" {
			continue
		}
		parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
	}
	return strings.Join(parts, "-")
}

// extractSourceIP: lấy IP nguồn từ X-Forwarded-For / X-Real-IP / RemoteAddr.
func extractSourceIP(r *http.Request) string {
	// Prefer explicit forwarded headers; fall back to RemoteAddr.
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			if ip := strings.TrimSpace(parts[0]); ip != "" {
				return ip
			}
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	// RemoteAddr might already be IP without port.
	return r.RemoteAddr
}

// bytesToText: nếu UTF-8 thì trả string; nếu binary thì base64 + tag "base64".
func bytesToText(b []byte) (string, string) {
	if len(b) == 0 {
		return "", ""
	}
	if utf8.Valid(b) {
		return string(b), ""
	}
	return base64.StdEncoding.EncodeToString(b), "base64"
}


