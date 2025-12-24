// http.ingest (raw mode)
// - Nhận HTTP request -> dump raw HTTP (text) -> forward sang AI_URL.
// - Không in raw ra stdout trừ khi bật PRINT_RAW=true.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"strings"
	"time"
)

// logState: thống kê nhẹ để /logcheck kiểm tra "có đang nhận & log request không".
// (in-memory, không ghi disk; reset khi container restart)
type logState struct {
	startedAt time.Time
	reqTotal  uint64
	fwdTotal  uint64
	dropTotal uint64

	mu          sync.Mutex
	lastAt      time.Time
	lastMethod  string
	lastTarget  string
	lastAIAt    time.Time
	lastAIError string
}

// main: start HTTP server; nhận request -> dump raw -> enqueue forward AI (không block).
func main() {
	port := getenv("PORT", "9002")
	st := &logState{startedAt: time.Now().UTC()}

	aiURL := strings.TrimSpace(getenv("AI_URL", ""))
	aiMode := strings.TrimSpace(getenv("AI_MODE", "llamacpp_chat")) // llamacpp_chat | llamacpp_completion | plain
	aiModel := strings.TrimSpace(getenv("AI_MODEL", "model.gguf"))
	aiMaxTokens := getenvInt("AI_MAX_TOKENS", 128)
	resultsFile := strings.TrimSpace(getenv("RESULTS_FILE", ""))
	queueSize := getenvInt("QUEUE_SIZE", 256)
	printRaw := strings.EqualFold(getenv("PRINT_RAW", "false"), "true")
	printResult := strings.EqualFold(getenv("PRINT_RESULT", "true"), "true")

	var q chan []byte
	if aiURL != "" {
		if queueSize < 1 {
			queueSize = 1
		}
		q = make(chan []byte, queueSize)
		go aiWorker(q, aiURL, aiMode, aiModel, aiMaxTokens, resultsFile, st, printResult)
	}

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
		fwd := atomic.LoadUint64(&st.fwdTotal)
		drop := atomic.LoadUint64(&st.dropTotal)
		uptime := now.Sub(st.startedAt)

		st.mu.Lock()
		lastAt := st.lastAt
		lastMethod := st.lastMethod
		lastTarget := st.lastTarget
		lastAIAt := st.lastAIAt
		lastAIError := st.lastAIError
		st.mu.Unlock()

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "status=ok\n")
		_, _ = fmt.Fprintf(w, "started_at=%s\n", st.startedAt.Format(time.RFC3339))
		_, _ = fmt.Fprintf(w, "uptime_seconds=%d\n", int64(uptime.Seconds()))
		_, _ = fmt.Fprintf(w, "requests_total=%d\n", total)
		_, _ = fmt.Fprintf(w, "forwarded_total=%d\n", fwd)
		_, _ = fmt.Fprintf(w, "dropped_total=%d\n", drop)
		_, _ = fmt.Fprintf(w, "ai_url=%s\n", aiURL)
		if !lastAt.IsZero() {
			_, _ = fmt.Fprintf(w, "last_request_at=%s\n", lastAt.Format(time.RFC3339Nano))
			_, _ = fmt.Fprintf(w, "last_request=%s %s\n", lastMethod, lastTarget)
		}
		if !lastAIAt.IsZero() {
			_, _ = fmt.Fprintf(w, "last_ai_at=%s\n", lastAIAt.Format(time.RFC3339Nano))
		}
		if lastAIError != "" {
			_, _ = fmt.Fprintf(w, "last_ai_error=%s\n", lastAIError)
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()

		// Read full request body (no limit) to preserve raw.
		bodyBytes, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		rawDump := dumpRawRequest(r, bodyBytes)

		atomic.AddUint64(&st.reqTotal, 1)
		st.mu.Lock()
		st.lastAt = time.Now().UTC()
		st.lastMethod = r.Method
		st.lastTarget = r.URL.Path
		if r.URL.RawQuery != "" {
			st.lastTarget = st.lastTarget + "?" + r.URL.RawQuery
		}
		st.mu.Unlock()

		if printRaw {
			_, _ = fmt.Fprintln(os.Stdout, "----- RAW REQUEST -----")
			_, _ = fmt.Fprintln(os.Stdout, rawDump)
			_, _ = fmt.Fprintln(os.Stdout, "")
		}

		if q != nil {
			select {
			case q <- []byte(rawDump):
				// queued
			default:
				atomic.AddUint64(&st.dropTotal, 1)
			}
		}

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

// dumpRawRequest: dump raw HTTP request (headers + body) bằng stdlib.
func dumpRawRequest(r *http.Request, body []byte) string {
	rr := new(http.Request)
	*rr = *r
	rr.Body = io.NopCloser(bytes.NewReader(body))
	b, err := httputil.DumpRequest(rr, true)
	if err != nil {
		target := r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		return fmt.Sprintf("%s %s HTTP/1.1\r\n\r\n", r.Method, target)
	}
	return string(b)
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
func getenvInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// aiWorker: nhận raw request, POST sang AI_URL, in/ghi kết quả.
func aiWorker(q <-chan []byte, aiURL string, aiMode string, aiModel string, aiMaxTokens int, resultsFile string, st *logState, printResult bool) {
	client := &http.Client{Timeout: 20 * time.Second}
	for raw := range q {
		atomic.AddUint64(&st.fwdTotal, 1)
		respBody, err := callAI(client, aiURL, aiMode, aiModel, aiMaxTokens, raw)

		st.mu.Lock()
		st.lastAIAt = time.Now().UTC()
		if err != nil {
			st.lastAIError = err.Error()
		} else {
			st.lastAIError = ""
		}
		st.mu.Unlock()

		if err != nil {
			if printResult {
				_, _ = fmt.Fprintf(os.Stdout, "----- AI ERROR -----\n%v\n\n", err)
			}
			appendResult(resultsFile, []byte(fmt.Sprintf("AI_ERROR: %v\n", err)))
			continue
		}

		if printResult {
			_, _ = fmt.Fprintln(os.Stdout, "----- AI RESULT -----")
			_, _ = fmt.Fprintln(os.Stdout, string(respBody))
			_, _ = fmt.Fprintln(os.Stdout, "")
		}
		appendResult(resultsFile, respBody)
	}
}

func callAI(client *http.Client, aiURL string, aiMode string, aiModel string, aiMaxTokens int, raw []byte) ([]byte, error) {
	return callAIWithMode(client, aiURL, aiMode, aiModel, aiMaxTokens, raw)
}

func callAIWithMode(client *http.Client, aiURL string, aiMode string, aiModel string, aiMaxTokens int, raw []byte) ([]byte, error) {
	aiMode = strings.ToLower(strings.TrimSpace(aiMode))
	var payload []byte
	var err error

	switch aiMode {
	case "", "plain":
		payload = raw
	case "llamacpp_completion":
		// POST /completion
		body := map[string]any{
			"prompt":    string(raw),
			"n_predict": aiMaxTokens,
			"stream":    false,
		}
		payload, err = json.Marshal(body)
	case "llamacpp_chat":
		// POST /v1/chat/completions (OpenAI-like)
		body := map[string]any{
			"model": aiModel,
			"messages": []map[string]string{
				{"role": "user", "content": string(raw)},
			},
			"max_tokens": aiMaxTokens,
			"stream":     false,
		}
		payload, err = json.Marshal(body)
	default:
		return nil, fmt.Errorf("unsupported AI_MODE: %s", aiMode)
	}
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", aiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	// Avoid gzip issues with llama.cpp
	req.Header.Set("Accept-Encoding", "identity")
	if aiMode == "plain" {
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB max result
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("AI returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// appendResult: nếu RESULTS_FILE set thì append vào file (best-effort).
func appendResult(path string, data []byte) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(data)
	if len(data) == 0 || data[len(data)-1] != '\n' {
		_, _ = f.Write([]byte("\n"))
	}
}