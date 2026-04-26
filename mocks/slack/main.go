// Minimal Slack webhook mock. Accepts POST /webhook, stores the last N
// payloads in memory, and exposes GET /received so reviewers can inspect
// what was "sent" without touching a real Slack workspace.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

type received struct {
	At      time.Time       `json:"at"`
	Headers http.Header     `json:"headers"`
	Body    json.RawMessage `json:"body"`
}

var (
	mu   sync.Mutex
	logs []received
)

const maxKept = 200

func main() {
	http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !json.Valid(body) {
			body, _ = json.Marshal(map[string]string{"raw": string(body)})
		}
		mu.Lock()
		logs = append(logs, received{At: time.Now().UTC(), Headers: r.Header.Clone(), Body: body})
		if len(logs) > maxKept {
			logs = logs[len(logs)-maxKept:]
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})

	http.HandleFunc("/received", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(logs)
	})

	http.HandleFunc("/clear", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		logs = nil
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	log.Println("slack-mock listening on :9000")
	if err := http.ListenAndServe(":9000", nil); err != nil {
		log.Fatal(err)
	}
}
