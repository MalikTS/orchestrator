package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
	"net"
	"bytes"

	"orchestrator/internal/types"
)

var (
	port = flag.Int("port", 8080, "controller HTTP port")
)


type Node struct {
	IP        string    `json:"ip"`
	LastPing  time.Time `json:"last_ping"`
	IsAlive   bool      `json:"is_alive"`
}


type Replica struct {
	ID     string `json:"id"`
	NodeIP string `json:"node_ip"`
	Port   int    `json:"port"`
}

type Controller struct {
	mu               sync.RWMutex
	nodes            map[string]*Node           
	replicas         map[string]*Replica       
	desiredReplicas  int
	nextReplicaID    int
	replicaCounter   int 
}

func NewController() *Controller {
	return &Controller{
		nodes:           make(map[string]*Node),
		replicas:        make(map[string]*Replica),
		desiredReplicas: 0,
		nextReplicaID:   1,
	}
}

func (c *Controller) generateReplicaID() string {
	id := fmt.Sprintf("replica-%d", c.replicaCounter)
	c.replicaCounter++
	return id
}


func (c *Controller) RegisterHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeIP string `json:"node_ip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.NodeIP == "" {
		http.Error(w, "missing node_ip", http.StatusBadRequest)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.nodes[req.NodeIP] = &Node{
		IP:       req.NodeIP,
		LastPing: time.Now(),
		IsAlive:  true,
	}
	slog.Info("Node registered", "ip", req.NodeIP)
	w.WriteHeader(http.StatusOK)
}


func (c *Controller) PingHandler(w http.ResponseWriter, r *http.Request) {
	nodeIP := r.RemoteAddr 
	
	if host, _, err := net.SplitHostPort(nodeIP); err == nil {
		nodeIP = host
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if node, exists := c.nodes[nodeIP]; exists {
		node.LastPing = time.Now()
		node.IsAlive = true
	} else {
		
		c.nodes[nodeIP] = &Node{
			IP:       nodeIP,
			LastPing: time.Now(),
			IsAlive:  true,
		}
		slog.Info("Node auto-registered via ping", "ip", nodeIP)
	}
	w.WriteHeader(http.StatusOK)
}


func (c *Controller) DeployHandler(w http.ResponseWriter, r *http.Request) {
	var req types.DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	c.mu.Lock()
	c.desiredReplicas = req.Replicas
	c.mu.Unlock()

	slog.Info("Deploy request", "desired_replicas", req.Replicas)
	c.reconcile()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{"desired_replicas": req.Replicas})
}


func (c *Controller) StatusHandler(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	
	var statuses []types.ReplicaStatus
	for _, rep := range c.replicas {
		statuses = append(statuses, types.ReplicaStatus{
			ID:       rep.ID,
			NodeIP:   rep.NodeIP,
			Port:     rep.Port,
			Uptime:   "unknown", 
			StartedAt: time.Now().Unix(), 
		})
	}

	resp := map[string]interface{}{
		"desired_replicas": c.desiredReplicas,
		"actual_replicas":  len(c.replicas),
		"replicas":         statuses,
		"nodes":            c.nodes,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}


func (c *Controller) reconcile() {
	c.mu.Lock()
	defer c.mu.Unlock()

	actual := len(c.replicas)
	desired := c.desiredReplicas

	if actual == desired {
		return
	}

	var aliveNodes []string
	for ip, node := range c.nodes {
		if time.Since(node.LastPing) < 30*time.Second { 
			aliveNodes = append(aliveNodes, ip)
		} else {
			node.IsAlive = false
		}
	}

	if len(aliveNodes) == 0 {
		slog.Warn("No alive nodes to schedule replicas")
		return
	}

	if actual < desired {
		for i := actual; i < desired; i++ {
			nodeIP := aliveNodes[i%len(aliveNodes)] 
			replicaID := c.generateReplicaID()

			
			go c.startReplicaOnNode(nodeIP, replicaID)
			c.replicas[replicaID] = &Replica{
				ID:     replicaID,
				NodeIP: nodeIP,
				Port:   0, 
			}
		}
	}

	
	if actual > desired {
		toRemove := actual - desired
		i := 0
		for id := range c.replicas {
			if i >= toRemove {
				break
			}
			go c.stopReplica(id)
			delete(c.replicas, id)
			i++
		}
	}
}


func (c *Controller) startReplicaOnNode(nodeIP, replicaID string) {
	url := fmt.Sprintf("http://%s:9001/start", nodeIP)
	reqBody := types.StartServiceRequest{
		ServiceID: replicaID,
		Port:      0,
		Config:    nil,
	}

	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Error("Failed to start replica on node", "node", nodeIP, "error", err)
		
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Error("Agent returned error", "node", nodeIP, "status", resp.Status)
	}
}


func (c *Controller) stopReplica(replicaID string) {
	
	var nodeIP string
	c.mu.RLock()
	if rep, ok := c.replicas[replicaID]; ok {
		nodeIP = rep.NodeIP
	}
	c.mu.RUnlock()

	if nodeIP == "" {
		return
	}

	url := fmt.Sprintf("http://%s:9001/stop?id=%s", nodeIP, replicaID)
	_, err := http.Get(url) 
	if err != nil {
		slog.Error("Failed to stop replica", "replica", replicaID, "error", err)
	}
}


func (c *Controller) startHealthChecker() {
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for range ticker.C {
			c.checkNodeHealth()
			c.reconcile()
		}
	}()
}

func (c *Controller) checkNodeHealth() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, node := range c.nodes {
		if time.Since(node.LastPing) > 30*time.Second {
			node.IsAlive = false
		}
	}
}

func main() {
	flag.Parse()

	ctrl := NewController()
	ctrl.startHealthChecker()

	http.HandleFunc("/register", ctrl.RegisterHandler)
	http.HandleFunc("/ping", ctrl.PingHandler)
	http.HandleFunc("/deploy", ctrl.DeployHandler)
	http.HandleFunc("/status", ctrl.StatusHandler)

	addr := fmt.Sprintf(":%d", *port)
	slog.Info("Controller starting", "addr", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		slog.Error("Controller failed", "error", err)
	}
}