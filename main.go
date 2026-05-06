package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ─── 数据结构 ────────────────────────────────────────────────────

type PeerInfo struct {
	InstanceID string   `json:"instance_id"`
	Nickname   string   `json:"nickname"`
	UserID     string   `json:"user_id,omitempty"`
	AvatarURL  string   `json:"avatar_url,omitempty"`
	Agents     []Agent  `json:"agents"`
	Addr       string   `json:"-"`
	conn       *websocket.Conn
	lastSeen   time.Time
	mu         sync.Mutex
}

type Agent struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	CapabilityTags []string `json:"capability_tags"`
	AuthLevel      string   `json:"auth_level"`
}

type SignalMessage struct {
	Type string          `json:"type"`
	From string          `json:"from,omitempty"`
	To   string          `json:"to,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
	HMAC string          `json:"hmac,omitempty"` // 消息完整性签名
	TS   int64           `json:"ts,omitempty"`   // 时间戳（防重放）
}

func computeHMAC(secret, msgType, from, to string, data []byte, ts int64) string {
	if secret == "" {
		return ""
	}
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(msgType))
	h.Write([]byte(from))
	h.Write([]byte(to))
	h.Write(data)
	h.Write([]byte(fmt.Sprintf("%d", ts)))
	return hex.EncodeToString(h.Sum(nil))
}

func verifyHMAC(msg SignalMessage) bool {
	if serverSecret == "" {
		return true
	}
	if msg.HMAC == "" {
		return false
	}
	if time.Now().Unix()-msg.TS > 300 {
		return false
	}
	expected := computeHMAC(serverSecret, msg.Type, msg.From, msg.To, msg.Data, msg.TS)
	return hmac.Equal([]byte(expected), []byte(msg.HMAC))
}

// ─── Hub 管理所有连接 ────────────────────────────────────────────

type Hub struct {
	mu    sync.RWMutex
	peers map[string]*PeerInfo
}

var hub = &Hub{peers: make(map[string]*PeerInfo)}

var allowedOrigins = os.Getenv("ALLOWED_ORIGINS") // 逗号分隔，空值允许所有

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		if allowedOrigins == "" {
			return true
		}
		origin := r.Header.Get("Origin")
		for _, o := range strings.Split(allowedOrigins, ",") {
			if strings.TrimSpace(o) == origin {
				return true
			}
		}
		return origin == ""
	},
}

var serverSecret = os.Getenv("SIGNALING_SECRET") // HMAC 签名密钥

func (h *Hub) register(p *PeerInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.peers[p.InstanceID]; ok {
		old.mu.Lock()
		old.conn.Close()
		old.mu.Unlock()
	}
	h.peers[p.InstanceID] = p
	log.Printf("[hub] registered: %s (%s)", p.InstanceID, p.Nickname)
}

func (h *Hub) unregister(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.peers, id)
	log.Printf("[hub] unregistered: %s", id)
}

func (h *Hub) getPeer(id string) *PeerInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.peers[id]
}

func (h *Hub) listOnlinePeers() []map[string]interface{} {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(h.peers))
	for _, p := range h.peers {
		out = append(out, map[string]interface{}{
			"instance_id": p.InstanceID,
			"nickname":    p.Nickname,
			"user_id":     p.UserID,
			"avatar_url":  p.AvatarURL,
			"agents":      p.Agents,
		})
	}
	return out
}

func (h *Hub) sendTo(targetID string, msg SignalMessage) error {
	h.mu.RLock()
	p := h.peers[targetID]
	h.mu.RUnlock()
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn.WriteJSON(msg)
}

// ─── WebSocket 处理 ──────────────────────────────────────────────

func handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
		return
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	var peer *PeerInfo
	defer func() {
		if peer != nil {
			hub.unregister(peer.InstanceID)
		}
	}()

	// Ping goroutine
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if peer != nil {
					peer.mu.Lock()
					peer.conn.WriteMessage(websocket.PingMessage, nil)
					peer.mu.Unlock()
				}
			}
		}
	}()

	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			break
		}
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		var msg SignalMessage
		if json.Unmarshal(msgBytes, &msg) != nil {
			continue
		}

		switch msg.Type {
		case "register":
			var reg struct {
				InstanceID string  `json:"instance_id"`
				Nickname   string  `json:"nickname"`
				UserID     string  `json:"user_id"`
				AvatarURL  string  `json:"avatar_url"`
				Agents     []Agent `json:"agents"`
			}
			json.Unmarshal(msg.Data, &reg)
			if reg.InstanceID == "" {
				continue
			}
			peer = &PeerInfo{
				InstanceID: reg.InstanceID,
				Nickname:   reg.Nickname,
				UserID:     reg.UserID,
				AvatarURL:  reg.AvatarURL,
				Agents:     reg.Agents,
				conn:       conn,
				lastSeen:   time.Now(),
			}
			hub.register(peer)
			conn.WriteJSON(SignalMessage{Type: "registered", Data: jsonRaw(map[string]string{"status": "ok"})})

		case "list_peers":
			peers := hub.listOnlinePeers()
			conn.WriteJSON(SignalMessage{Type: "peers_list", Data: jsonRaw(peers)})

		case "signal":
			if msg.To == "" || peer == nil {
				continue
			}
			if serverSecret != "" && !verifyHMAC(msg) {
				log.Printf("[ws] HMAC verification failed for signal from %s", peer.InstanceID)
				continue
			}
			msg.From = peer.InstanceID
			hub.sendTo(msg.To, msg)

		case "connect_request":
			if msg.To == "" || peer == nil {
				continue
			}
			if serverSecret != "" && !verifyHMAC(msg) {
				log.Printf("[ws] HMAC verification failed for connect_request from %s", peer.InstanceID)
				continue
			}
			msg.From = peer.InstanceID
			hub.sendTo(msg.To, msg)

		case "connect_respond":
			if msg.To == "" || peer == nil {
				continue
			}
			if serverSecret != "" && !verifyHMAC(msg) {
				log.Printf("[ws] HMAC verification failed for connect_respond from %s", peer.InstanceID)
				continue
			}
			msg.From = peer.InstanceID
			hub.sendTo(msg.To, msg)

		case "relay":
			if msg.To == "" || peer == nil {
				continue
			}
			if serverSecret != "" && !verifyHMAC(msg) {
				log.Printf("[ws] HMAC verification failed for relay from %s", peer.InstanceID)
				continue
			}
			msg.From = peer.InstanceID
			hub.sendTo(msg.To, msg)

		case "heartbeat":
			if peer != nil {
				peer.lastSeen = time.Now()
			}
			conn.WriteJSON(SignalMessage{Type: "heartbeat_ack"})
		}
	}
}

func jsonRaw(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ─── HTTP 端点 ───────────────────────────────────────────────────

func handlePeers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(hub.listOnlinePeers())
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	hub.mu.RLock()
	count := len(hub.peers)
	hub.mu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "online": count})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9090"
	}

	http.HandleFunc("/ws", handleWS)
	http.HandleFunc("/api/peers", handlePeers)
	http.HandleFunc("/health", handleHealth)

	tlsCert := os.Getenv("TLS_CERT")
	tlsKey := os.Getenv("TLS_KEY")

	if tlsCert != "" && tlsKey != "" {
		log.Printf("[signaling] server starting on :%s (TLS/WSS)", port)
		if err := http.ListenAndServeTLS(":"+port, tlsCert, tlsKey, nil); err != nil {
			log.Fatalf("[signaling] server error: %v", err)
		}
	} else {
		log.Printf("[signaling] server starting on :%s (plain WS, set TLS_CERT & TLS_KEY for WSS)", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Fatalf("[signaling] server error: %v", err)
		}
	}
}
