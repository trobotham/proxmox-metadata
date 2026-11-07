package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const tokenHeader = "X-aws-ec2-metadata-token"

type Config struct {
	Listen             string     `json:"listen"`
	RequireToken       *bool      `json:"require_token,omitempty"`
	MaxTokenTTLSeconds int        `json:"max_token_ttl_seconds,omitempty"`
	Instances          []Instance `json:"instances"`
}

type Instance struct {
	Sources       []string          `json:"sources"`
	InstanceID    string            `json:"instance_id"`
	VMID          int               `json:"vm_id"`
	Hostname      string            `json:"hostname"`
	PublicIPv4    string            `json:"public_ipv4,omitempty"`
	IPv6          string            `json:"ipv6,omitempty"`
	IPv6Prefix    string            `json:"ipv6_prefix,omitempty"`
	GatewayIPv4   string            `json:"gateway_ipv4,omitempty"`
	GatewayIPv6   string            `json:"gateway_ipv6,omitempty"`
	IPv4Networks  []IPv4Network     `json:"ipv4_networks,omitempty"`
	NetworkInterfaces []NetworkInterface `json:"network_interfaces,omitempty"`
	Proxmox       map[string]any    `json:"proxmox,omitempty"`
	Kea           map[string]any    `json:"kea,omitempty"`
	UserData      string            `json:"user_data,omitempty"`
	CustomMetadata map[string]string `json:"custom_metadata,omitempty"`
	networks      []*net.IPNet
}

type NetworkInterface struct {
	Name     string `json:"name"`
	Model    string `json:"model,omitempty"`
	MAC      string `json:"mac,omitempty"`
	Bridge   string `json:"bridge,omitempty"`
	VLAN     string `json:"vlan,omitempty"`
	Firewall string `json:"firewall,omitempty"`
	MTU      string `json:"mtu,omitempty"`
	Rate     string `json:"rate,omitempty"`
}

type IPv4Network struct {
	Address string `json:"address"`
	Prefix  string `json:"prefix"`
	Gateway string `json:"gateway"`
	DHCPMAC string `json:"dhcp_mac,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

type runtimeConfig struct {
	listen       string
	requireToken bool
	maxTokenTTL  time.Duration
	instances    []Instance
}

type tokenRecord struct {
	source  string
	expires time.Time
}

type server struct {
	configPath string
	log        *slog.Logger
	cfgMu      sync.RWMutex
	cfg        *runtimeConfig
	tokenMu    sync.Mutex
	tokens     map[string]tokenRecord
}

func main() {
	configPath := flag.String("config", "/etc/proxmox-metadata/config.json", "configuration file")
	check := flag.Bool("check", false, "validate configuration and exit")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadConfig(*configPath)
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}
	if *check {
		fmt.Println("configuration is valid")
		return
	}

	s := &server{configPath: *configPath, log: logger, cfg: cfg, tokens: make(map[string]tokenRecord)}
	httpServer := &http.Server{
		Addr:              cfg.listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	go func() {
		for range reload {
			if err := s.reload(); err != nil {
				s.log.Error("configuration reload failed", "error", err)
			} else {
				s.log.Info("configuration reloaded")
			}
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("metadata service starting", "listen", cfg.listen, "token_required", cfg.requireToken)
	err = httpServer.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (*runtimeConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	if raw.Listen == "" {
		raw.Listen = metadataIP + ":80"
	}
	if err := validateListen(raw.Listen); err != nil {
		return nil, err
	}
	requireToken := true
	if raw.RequireToken != nil {
		requireToken = *raw.RequireToken
	}
	if raw.MaxTokenTTLSeconds == 0 {
		raw.MaxTokenTTLSeconds = 21600
	}
	if raw.MaxTokenTTLSeconds < 1 || raw.MaxTokenTTLSeconds > 21600 {
		return nil, fmt.Errorf("max_token_ttl_seconds must be between 1 and 21600")
	}
	if len(raw.Instances) == 0 {
		return nil, fmt.Errorf("at least one instance is required")
	}
	ids := make(map[string]bool)
	for i := range raw.Instances {
		inst := &raw.Instances[i]
		if inst.InstanceID == "" || inst.Hostname == "" || inst.VMID <= 0 || len(inst.Sources) == 0 {
			return nil, fmt.Errorf("instance %d requires instance_id, vm_id, hostname, and sources", i)
		}
		if ids[inst.InstanceID] {
			return nil, fmt.Errorf("duplicate instance_id %q", inst.InstanceID)
		}
		ids[inst.InstanceID] = true
		for _, source := range inst.Sources {
			n, err := parseSource(source)
			if err != nil {
				return nil, fmt.Errorf("instance %q source %q: %w", inst.InstanceID, source, err)
			}
			inst.networks = append(inst.networks, n)
		}
	}
	return &runtimeConfig{
		listen: raw.Listen, requireToken: requireToken,
		maxTokenTTL: time.Duration(raw.MaxTokenTTLSeconds) * time.Second,
		instances: raw.Instances,
	}, nil
}

// metadataIP is the only address the service may bind. Binding a wildcard
// address would capture port 80 on every host interface.
const metadataIP = "169.254.169.254"

func validateListen(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", listen, err)
	}
	if host != metadataIP {
		return fmt.Errorf("listen %q: host must be %s", listen, metadataIP)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("listen %q: invalid port", listen)
	}
	return nil
}

func parseSource(value string) (*net.IPNet, error) {
	if !strings.Contains(value, "/") {
		ip := net.ParseIP(value)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP address")
		}
		bits := 128
		if ip.To4() != nil {
			bits = 32
		}
		value += "/" + strconv.Itoa(bits)
	}
	_, n, err := net.ParseCIDR(value)
	return n, err
}

func (s *server) reload() error {
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return err
	}
	s.cfgMu.Lock()
	if cfg.listen != s.cfg.listen {
		s.cfgMu.Unlock()
		return fmt.Errorf("listen address cannot be changed by SIGHUP; restart the service")
	}
	s.cfg = cfg
	s.cfgMu.Unlock()
	return nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("PUT /latest/api/token", s.createToken)
	mux.HandleFunc("GET /latest/meta-data", s.metadataRoot)
	mux.HandleFunc("GET /latest/meta-data/", s.metadata)
	mux.HandleFunc("GET /latest/user-data", s.userData)
	return s.securityHeaders(s.accessLog(mux))
}

func (s *server) sourceIP(r *http.Request) (net.IP, string, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil, "", err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, "", fmt.Errorf("invalid remote address")
	}
	return ip, ip.String(), nil
}

func (s *server) instanceFor(r *http.Request) (*Instance, string, error) {
	ip, source, err := s.sourceIP(r)
	if err != nil {
		return nil, "", err
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	var best *Instance
	bestPrefix := -1
	for i := range s.cfg.instances {
		for _, n := range s.cfg.instances[i].networks {
			if n.Contains(ip) {
				ones, _ := n.Mask.Size()
				if ones > bestPrefix {
					best = &s.cfg.instances[i]
					bestPrefix = ones
				} else if ones == bestPrefix && best != &s.cfg.instances[i] {
					return nil, source, fmt.Errorf("ambiguous source mapping")
				}
			}
		}
	}
	if best == nil {
		return nil, source, fmt.Errorf("unmapped source")
	}
	return best, source, nil
}

func (s *server) createToken(w http.ResponseWriter, r *http.Request) {
	_, source, err := s.instanceFor(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ttlSeconds, err := strconv.Atoi(r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds"))
	s.cfgMu.RLock()
	maxTTL := s.cfg.maxTokenTTL
	s.cfgMu.RUnlock()
	if err != nil || ttlSeconds < 1 || time.Duration(ttlSeconds)*time.Second > maxTTL {
		http.Error(w, "invalid token TTL", http.StatusBadRequest)
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	s.tokenMu.Lock()
	for key, record := range s.tokens {
		if time.Now().After(record.expires) {
			delete(s.tokens, key)
		}
	}
	s.tokens[token] = tokenRecord{source: source, expires: time.Now().Add(time.Duration(ttlSeconds) * time.Second)}
	s.tokenMu.Unlock()
	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprintln(w, token)
}

func (s *server) authorized(r *http.Request, source string) bool {
	s.cfgMu.RLock()
	required := s.cfg.requireToken
	s.cfgMu.RUnlock()
	if !required {
		return true
	}
	token := r.Header.Get(tokenHeader)
	if token == "" {
		return false
	}
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	record, ok := s.tokens[token]
	if !ok || record.source != source || time.Now().After(record.expires) {
		delete(s.tokens, token)
		return false
	}
	return true
}

func (s *server) metadataRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/latest/meta-data/", http.StatusMovedPermanently)
}

func (s *server) metadata(w http.ResponseWriter, r *http.Request) {
	inst, source, err := s.instanceFor(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !s.authorized(r, source) {
		http.Error(w, "missing or invalid metadata token", http.StatusUnauthorized)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/latest/meta-data/")
	if key == "network/ipv4" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(inst.IPv4Networks)
		return
	}
	if key == "network/interfaces" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(inst.NetworkInterfaces)
		return
	}
	if key == "proxmox" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(inst.Proxmox)
		return
	}
	if key == "kea" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(inst.Kea)
		return
	}
	if key == "identity-document" {
		document := map[string]any{
			"instanceId": inst.InstanceID, "vmId": inst.VMID,
			"hostname": inst.Hostname, "publicIpv4": inst.PublicIPv4,
			"ipv6": inst.IPv6, "ipv6Prefix": inst.IPv6Prefix,
			"proxmox": inst.Proxmox, "kea": inst.Kea,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(document)
		return
	}
	if key == "ipv4-addresses" {
		values := make([]string, 0, len(inst.IPv4Networks))
		for _, network := range inst.IPv4Networks {
			values = append(values, network.Address)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintln(w, strings.Join(values, "\n"))
		return
	}
	if key == "ipv4-gateways" {
		values := make([]string, 0, len(inst.IPv4Networks))
		for _, network := range inst.IPv4Networks {
			values = append(values, network.Prefix+" "+network.Gateway)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintln(w, strings.Join(values, "\n"))
		return
	}
	values := map[string]string{
		"instance-id": inst.InstanceID, "local-hostname": inst.Hostname,
		"vm-id": strconv.Itoa(inst.VMID), "public-ipv4": inst.PublicIPv4,
		"ipv6": inst.IPv6, "ipv6-prefix": inst.IPv6Prefix,
		"gateway-ipv4": inst.GatewayIPv4, "gateway-ipv6": inst.GatewayIPv6,
	}
	for k, v := range inst.CustomMetadata {
		values["custom/"+k] = v
	}
	if key == "" {
		keys := make([]string, 0, len(values))
		for k, v := range values {
			if v != "" {
				keys = append(keys, k)
			}
		}
		if len(inst.IPv4Networks) > 0 {
			keys = append(keys, "ipv4-addresses", "ipv4-gateways", "network/ipv4")
		}
		if len(inst.NetworkInterfaces) > 0 { keys = append(keys, "network/interfaces") }
		if len(inst.Proxmox) > 0 { keys = append(keys, "proxmox") }
		if len(inst.Kea) > 0 { keys = append(keys, "kea") }
		keys = append(keys, "identity-document")
		sort.Strings(keys)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintln(w, strings.Join(keys, "\n"))
		return
	}
	value, ok := values[key]
	if !ok || value == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprintln(w, value)
}

func (s *server) userData(w http.ResponseWriter, r *http.Request) {
	inst, source, err := s.instanceFor(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !s.authorized(r, source) {
		http.Error(w, "missing or invalid metadata token", http.StatusUnauthorized)
		return
	}
	if inst.UserData == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprint(w, inst.UserData)
}

func (s *server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		_, source, _ := s.sourceIP(r)
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "source", source, "duration_ms", time.Since(start).Milliseconds())
	})
}
