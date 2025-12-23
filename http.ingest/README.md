## `http.ingest` (raw forwarder)

Receives HTTP requests (from `goreplay`) and **dumps raw HTTP** via `httputil.DumpRequest`, then forwards that raw text
to an internal AI service for analysis.

### Endpoints

- `GET /healthz`: returns `ok`
- `GET /logcheck`: status counters (uptime / total requests / forwarded / dropped / last AI error)

### Environment

- `PORT` (default: `9002`)
- `AI_URL` (default: empty) – if set, raw requests are POSTed to this URL as `text/plain`
- `QUEUE_SIZE` (default: `256`) – async forward queue size (drops when full)
- `PRINT_RAW` (default: `true`) – print raw HTTP blocks to stdout
- `PRINT_RESULT` (default: `true`) – print AI response to stdout
- `RESULTS_FILE` (default: empty) – if set, append AI responses to this file inside the container


