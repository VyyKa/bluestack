// http.ingest (raw mode)
// - Receive HTTP requests -> dump raw HTTP (text) -> forward to AI_URL (async).
// - Optionally print RAW REQUEST blocks (PRINT_RAW=true).
// - Print compact AI RESULT blocks: {"verdict": "...", "snippets":[...]}.
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

// aiCompactResult is what we print in compact mode.
type aiCompactResult struct {
	Verdict  string   `json:"verdict"`
	Snippets []string `json:"snippets,omitempty"`
}

// logState is in-memory status for /logcheck (resets on container restart).
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

// main starts the HTTP server and an optional async AI forwarder.
func main() {
	port := getenv("PORT", "9002")
	st := &logState{startedAt: time.Now().UTC()}

	aiURL := getenv("AI_URL", "")
	aiMode := getenv("AI_MODE", "llamacpp_chat") // llamacpp_chat | llamacpp_completion | plain
	aiModel := getenv("AI_MODEL", "model.gguf")
	aiMaxTokens := getenvInt("AI_MAX_TOKENS", 128)
	resultsFile := getenv("RESULTS_FILE", "")
	queueSize := getenvInt("QUEUE_SIZE", 256)
	printRaw := strings.EqualFold(getenv("PRINT_RAW", "false"), "true")
	printResult := strings.EqualFold(getenv("PRINT_RESULT", "true"), "true")
	printResultFormat := getenv("PRINT_RESULT_FORMAT", "compact") // compact | full

	var q chan []byte
	if aiURL != "" {
		if queueSize < 1 {
			queueSize = 1
		}
		q = make(chan []byte, queueSize)
		go aiWorker(q, aiURL, aiMode, aiModel, aiMaxTokens, resultsFile, st, printResult, printResultFormat)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	// Browsers may request /favicon.ico automatically; keep logs clean.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// /logcheck returns a small status page for debugging.
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

// dumpRawRequest dumps a raw HTTP request (headers + body) using the stdlib.
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

// getenv returns an env var, or a default if empty (trimmed).
func getenv(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

// getenvInt returns an int env var (invalid/<=0 => default).
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

// aiWorker receives raw HTTP text, POSTs to AI_URL, and prints/stores results.
func aiWorker(q <-chan []byte, aiURL string, aiMode string, aiModel string, aiMaxTokens int, resultsFile string, st *logState, printResult bool, printResultFormat string) {
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
			if strings.EqualFold(printResultFormat, "full") {
				_, _ = fmt.Fprintln(os.Stdout, "----- AI RESULT -----")
				_, _ = fmt.Fprintln(os.Stdout, string(respBody))
				_, _ = fmt.Fprintln(os.Stdout, "")
			} else {
				compact := extractVerdictAndSnippets(aiMode, respBody, string(raw))
				b, _ := json.Marshal(compact)
				_, _ = fmt.Fprintln(os.Stdout, "----- AI RESULT -----")
				_, _ = fmt.Fprintln(os.Stdout, string(b))
				_, _ = fmt.Fprintln(os.Stdout, "")
			}
		}
		// Always store the full response (if RESULTS_FILE is set), even in compact mode.
		appendResult(resultsFile, respBody)
	}
}

// extractVerdictAndSnippets parses only verdict+snippets from the AI response.
// For llamacpp_chat: reads `choices[0].message.content` (expected to be JSON).
// For llamacpp_completion: reads `content` (expected to be JSON).
// Fallbacks:
// - if content isn't JSON -> verdict=content, snippets=[]
// - if everything empty -> verdict="EMPTY"
func extractVerdictAndSnippets(aiMode string, respBody []byte, rawRequestText string) aiCompactResult {
	aiMode = strings.ToLower(strings.TrimSpace(aiMode))

	content := ""
	switch aiMode {
	case "llamacpp_chat":
		var v struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(respBody, &v) == nil && len(v.Choices) > 0 {
			content = strings.TrimSpace(v.Choices[0].Message.Content)
		}
	case "llamacpp_completion":
		var v struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(respBody, &v) == nil {
			content = strings.TrimSpace(v.Content)
		}
	default:
		// plain/unknown: treat the full body as content
		content = strings.TrimSpace(string(respBody))
	}

	// If the model returned JSON in content, parse just what we need.
	var out aiCompactResult
	if content != "" && json.Unmarshal([]byte(content), &out) == nil && strings.TrimSpace(out.Verdict) != "" {
		out.Verdict = strings.TrimSpace(out.Verdict)
		if len(out.Snippets) == 0 {
			out.Snippets = extractRequestSnippets(rawRequestText)
		}
		return out
	}

	// Fallback: verdict is the content (or response body).
	fallback := strings.TrimSpace(content)
	if fallback == "" {
		fallback = strings.TrimSpace(string(respBody))
	}
	if fallback == "" {
		return aiCompactResult{Verdict: "EMPTY"}
	}
	if len(fallback) > 200 {
		fallback = fallback[:200] + "..."
	}
	return aiCompactResult{
		Verdict:  fallback,
		Snippets: extractRequestSnippets(rawRequestText),
	}
}

// extractRequestSnippets picks a few meaningful lines from the raw HTTP request text.
// Intentionally allowlists common, safe headers (no Cookie/Authorization).
func extractRequestSnippets(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(raw, "\n")

	// request line
	var out []string
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		out = append(out, ln)
		break
	}

	// allowlisted headers (deterministic order)
	allow := []string{
		"host:",
		"user-agent:",
		"content-type:",
		"content-length:",
		"accept:",
		"accept-encoding:",
	}
	seen := map[string]bool{}

	for _, ln := range lines {
		lnTrim := strings.TrimSpace(ln)
		if lnTrim == "" {
			break // stop at header/body boundary
		}
		lower := strings.ToLower(lnTrim)
		for _, k := range allow {
			if strings.HasPrefix(lower, k) && !seen[k] {
				out = append(out, lnTrim)
				seen[k] = true
				break
			}
		}
	}

	// keep it short
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func callAI(client *http.Client, aiURL string, aiMode string, aiModel string, aiMaxTokens int, raw []byte) ([]byte, error) {
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

// appendResult appends to RESULTS_FILE if set (best-effort).
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