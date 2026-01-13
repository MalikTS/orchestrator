package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
	"bytes"

	"orchestrator/internal/types"
)

var (
	port    = flag.Int("port", 9000, "agent HTTP port")
	nodeIP  = flag.String("node-ip", "127.0.0.1", "public IP of this node")
	workloadBinary = flag.String("workload-bin", "./workload", "path to workload binary")
)

type Agent struct {
	mu       sync.RWMutex
	replicas map[string]*ReplicaProcess 
	nodeIP   string
	binPath  string
	nextPort int
}

type ReplicaProcess struct {
	Cmd      *exec.Cmd
	Port     int
	ServiceID string
	StartAt  time.Time
}

func NewAgent(nodeIP, binPath string) *Agent {
	return &Agent{
		replicas: make(map[string]*ReplicaProcess),
		nodeIP:   nodeIP,
		binPath:  binPath,
		nextPort: 8000, 
	}
}

func (a *Agent) getNextPort() int {
	a.nextPort++
	return a.nextPort - 1
}

func (a *Agent) StartReplica(serviceID string, config map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.replicas[serviceID]; exists {
		return fmt.Errorf("replica %s already running", serviceID)
	}

	port := a.getNextPort()
	cmd := exec.Command(a.binPath, "-port", strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start workload: %w", err)
	}

	a.replicas[serviceID] = &ReplicaProcess{
		Cmd:       cmd,
		Port:      port,
		ServiceID: serviceID,
		StartAt:   time.Now(),
	}

	slog.Info("Started replica", "service_id", serviceID, "port", port, "pid", cmd.Process.Pid)
	return nil
}

func (a *Agent) StopReplica(serviceID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	proc, exists := a.replicas[serviceID]
	if !exists {
		return fmt.Errorf("replica %s not found", serviceID)
	}

	if proc.Cmd.Process != nil {
		_ = proc.Cmd.Process.Signal(os.Interrupt)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- proc.Cmd.Wait() }()
		select {
		case <-done:
		case <-ctx.Done():
			_ = proc.Cmd.Process.Kill()
			<-done
		}
	}

	delete(a.replicas, serviceID)
	slog.Info("Stopped replica", "service_id", serviceID)
	return nil
}

func (a *Agent) StatusHandler(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var statuses []types.ReplicaStatus
	for _, proc := range a.replicas {
		statuses = append(statuses, types.ReplicaStatus{
			ID:        proc.ServiceID,
			NodeIP:    a.nodeIP,
			Port:      proc.Port,
			Uptime:    time.Since(proc.StartAt).String(),
			StartedAt: proc.StartAt.Unix(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statuses)
}

func (a *Agent) StartHandler(w http.ResponseWriter, r *http.Request) {
	var req types.StartServiceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if err := a.StartReplica(req.ServiceID, req.Config); err != nil {
		slog.Error("Failed to start replica", "error", err, "service_id", req.ServiceID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"started"}`))
}

func (a *Agent) StopHandler(w http.ResponseWriter, r *http.Request) {
	serviceID := r.URL.Query().Get("id")
	if serviceID == "" {
		http.Error(w, "missing 'id' query param", http.StatusBadRequest)
		return
	}

	if err := a.StopReplica(serviceID); err != nil {
		slog.Error("Failed to stop replica", "error", err, "service_id", serviceID)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"stopped"}`))
}

func main() {
	flag.Parse()

	agent := NewAgent(*nodeIP, *workloadBinary)

	// Регистрация
	go func() {
		time.Sleep(1 * time.Second)
		ctrlURL := "http://localhost:8080/register"
		payload, _ := json.Marshal(map[string]string{"node_ip": *nodeIP})
		_, err := http.Post(ctrlURL, "application/json", bytes.NewReader(payload))
		if err != nil {
			slog.Error("Failed to register with controller", "error", err)
		} else {
			slog.Info("Registered with controller", "controller", "http://localhost:8080")
		}
	}()

	// Пинг каждые 10 сек
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for range ticker.C {
			_, err := http.Get("http://localhost:8080/ping")
			if err != nil {
				slog.Warn("Failed to ping controller", "error", err)
			}
		}
	}()

	http.HandleFunc("/status", agent.StatusHandler)
	http.HandleFunc("/start", agent.StartHandler)
	http.HandleFunc("/stop", agent.StopHandler)

	addr := fmt.Sprintf(":%d", *port)
	slog.Info("Agent starting", "addr", addr, "node_ip", *nodeIP)
	if err := http.ListenAndServe(addr, nil); err != nil {
		slog.Error("Agent failed", "error", err)
		os.Exit(1)
	}
}