package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
)

var (
	port = flag.Int("port", 8000, "port to listen on")
)

func main() {
	flag.Parse()

	id := uuid.New().String()
	startTime := time.Now().Unix()

	slog.Info("Workload service starting", "id", id, "port", *port)

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"id":        id,
			"port":      *port,
			"uptime":    time.Since(time.Unix(startTime, 0)).String(),
			"started_at": startTime,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	addr := fmt.Sprintf(":%d", *port)
	slog.Info("Listening", "addr", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}