package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type SyncConfig struct {
	Pvesh              string              `json:"pvesh"`
	KeaDHCP4Config     string              `json:"kea_dhcp4_config"`
	Output             string              `json:"output"`
	Listen             string              `json:"listen"`
	RequireToken       bool                `json:"require_token"`
	MaxTokenTTLSeconds int                 `json:"max_token_ttl_seconds"`
	IPv6Gateway        string              `json:"ipv6_gateway,omitempty"`
	Overrides          map[string]Override `json:"overrides,omitempty"`
}

type Override struct {
	InstanceID     string            `json:"instance_id,omitempty"`
	Hostname       string            `json:"hostname,omitempty"`
	IPv6Prefix     string            `json:"ipv6_prefix,omitempty"`
	UserData       string            `json:"user_data,omitempty"`
	CustomMetadata map[string]string `json:"custom_metadata,omitempty"`
}

type Resource struct {
	VMID int    `json:"vmid"`
	Node string `json:"node"`
	Type string `json:"type"`
	Name string `json:"name"`
	Status string `json:"status"`
	Pool string `json:"pool"`
	Tags string `json:"tags"`
	Uptime float64 `json:"uptime"`
	MaxCPU float64 `json:"maxcpu"`
	MaxMem float64 `json:"maxmem"`
	Template float64 `json:"template"`
}

type IPSetEntry struct {
	CIDR    string `json:"cidr"`
	Nomatch any    `json:"nomatch,omitempty"`
}

type WebConfig struct {
	Listen             string        `json:"listen"`
	RequireToken       bool          `json:"require_token"`
	MaxTokenTTLSeconds int           `json:"max_token_ttl_seconds"`
	Instances          []WebInstance `json:"instances"`
}

type WebInstance struct {
	Sources        []string          `json:"sources"`
	InstanceID     string            `json:"instance_id"`
	VMID           int               `json:"vm_id"`
	Hostname       string            `json:"hostname"`
	PublicIPv4     string            `json:"public_ipv4,omitempty"`
	IPv6           string            `json:"ipv6,omitempty"`
	IPv6Prefix     string            `json:"ipv6_prefix,omitempty"`
	GatewayIPv4    string            `json:"gateway_ipv4,omitempty"`
	GatewayIPv6    string            `json:"gateway_ipv6,omitempty"`
	IPv4Networks   []IPv4Network     `json:"ipv4_networks,omitempty"`
	NetworkInterfaces []NetworkInterface `json:"network_interfaces,omitempty"`
	Proxmox        map[string]any    `json:"proxmox,omitempty"`
	Kea            map[string]any    `json:"kea,omitempty"`
	UserData       string            `json:"user_data,omitempty"`
	CustomMetadata map[string]string `json:"custom_metadata,omitempty"`
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
}

type subnetGateway struct {
	network *net.IPNet
	gateway string
	id      int
	nextServer string
	options map[string]string
}

func main() {
	configPath := flag.String("config", "/etc/proxmox-metadata/discovery.json", "discovery configuration")
	check := flag.Bool("check", false, "validate inputs without writing output")
	flag.Parse()

	cfg, err := loadSyncConfig(*configPath)
	fatalIf(err)
	gateways, err := loadKeaGateways(cfg.KeaDHCP4Config)
	fatalIf(err)
	resources, err := pveshResources(cfg.Pvesh)
	fatalIf(err)

	result := WebConfig{
		Listen: cfg.Listen, RequireToken: cfg.RequireToken,
		MaxTokenTTLSeconds: cfg.MaxTokenTTLSeconds,
	}
	for _, resource := range resources {
		if resource.Type != "qemu" && resource.Type != "lxc" {
			continue
		}
		entries, err := pveshIPSet(cfg.Pvesh, resource)
		if err != nil {
			// A missing ipfilter-net0 is normal. Other failures are printed so the
			// administrator can distinguish missing metadata from broken discovery.
			fmt.Fprintf(os.Stderr, "skip %s/%d: %v\n", resource.Type, resource.VMID, err)
			continue
		}
		guestConfig, _ := pveshObject(cfg.Pvesh, resource, "config")
		guestStatus, _ := pveshObject(cfg.Pvesh, resource, "status/current")
		instance, ok := makeInstance(resource, entries, gateways, guestConfig, guestStatus, cfg)
		if ok {
			result.Instances = append(result.Instances, instance)
		}
	}
	sort.Slice(result.Instances, func(i, j int) bool { return result.Instances[i].VMID < result.Instances[j].VMID })
	if len(result.Instances) == 0 {
		fatalIf(fmt.Errorf("discovery produced no instances; refusing to replace the runtime configuration"))
	}
	if *check {
		fmt.Printf("discovery is valid: %d instances would be written\n", len(result.Instances))
		return
	}
	fatalIf(writeAtomic(cfg.Output, result))
	fmt.Printf("wrote %d instances to %s\n", len(result.Instances), cfg.Output)
}

func pveshObject(binary string, r Resource, suffix string) (map[string]any, error) {
	path := fmt.Sprintf("/nodes/%s/%s/%d/%s", r.Node, r.Type, r.VMID, suffix)
	result := make(map[string]any)
	err := runJSON(binary, []string{"get", path, "--output-format", "json"}, &result)
	return result, err
}

func loadSyncConfig(path string) (SyncConfig, error) {
	var cfg SyncConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, err
	}
	if cfg.Pvesh == "" { cfg.Pvesh = "/usr/bin/pvesh" }
	if cfg.KeaDHCP4Config == "" { cfg.KeaDHCP4Config = "/etc/kea/kea-dhcp4.conf" }
	if cfg.Output == "" { cfg.Output = "/etc/proxmox-metadata/config.json" }
	if cfg.Listen == "" { cfg.Listen = "169.254.169.254:80" }
	if cfg.MaxTokenTTLSeconds == 0 { cfg.MaxTokenTTLSeconds = 21600 }
	if cfg.MaxTokenTTLSeconds < 1 || cfg.MaxTokenTTLSeconds > 21600 {
		return cfg, fmt.Errorf("max_token_ttl_seconds must be between 1 and 21600")
	}
	return cfg, nil
}

func pveshResources(binary string) ([]Resource, error) {
	var resources []Resource
	if err := runJSON(binary, []string{"get", "/cluster/resources", "--type", "vm", "--output-format", "json"}, &resources); err != nil {
		return nil, err
	}
	return resources, nil
}

func pveshIPSet(binary string, r Resource) ([]IPSetEntry, error) {
	path := fmt.Sprintf("/nodes/%s/%s/%d/firewall/ipset/ipfilter-net0", r.Node, r.Type, r.VMID)
	var entries []IPSetEntry
	if err := runJSON(binary, []string{"get", path, "--output-format", "json"}, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func runJSON(binary string, args []string, destination any) error {
	cmd := exec.Command(binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(stdout.Bytes(), destination); err != nil {
		return fmt.Errorf("decode pvesh output: %w", err)
	}
	return nil
}

func makeInstance(r Resource, entries []IPSetEntry, gateways []subnetGateway, guestConfig, guestStatus map[string]any, cfg SyncConfig) (WebInstance, bool) {
	instance := WebInstance{VMID: r.VMID, Hostname: r.Name, InstanceID: fmt.Sprintf("pve-%s-%d", r.Type, r.VMID), GatewayIPv6: cfg.IPv6Gateway}
	instance.Proxmox = proxmoxMetadata(r, guestConfig, guestStatus)
	instance.NetworkInterfaces = parseNetworkInterfaces(guestConfig)
	for _, entry := range entries {
		if entry.CIDR == "" || isTrue(entry.Nomatch) {
			continue
		}
		ip, network, err := parseIPOrCIDR(entry.CIDR)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip invalid ipset entry for VM %d: %s\n", r.VMID, entry.CIDR)
			continue
		}
		canonical := network.String()
		instance.Sources = append(instance.Sources, canonical)
		ones, bits := network.Mask.Size()
		if bits == 32 && ones == 32 {
			prefix, gateway := networkFor(ip, gateways)
			instance.IPv4Networks = append(instance.IPv4Networks, IPv4Network{Address: ip.String(), Prefix: prefix, Gateway: gateway})
		}
		if bits == 128 {
			if ones == 128 && instance.IPv6 == "" && !ip.IsLinkLocalUnicast() {
				instance.IPv6 = ip.String()
			}
			if ones == 64 && instance.IPv6Prefix == "" {
				instance.IPv6Prefix = canonical
			}
		}
	}
	override := cfg.Overrides[strconv.Itoa(r.VMID)]
	if override.InstanceID != "" { instance.InstanceID = override.InstanceID }
	if override.Hostname != "" { instance.Hostname = override.Hostname }
	if override.IPv6Prefix != "" { instance.IPv6Prefix = override.IPv6Prefix }
	instance.UserData = override.UserData
	instance.CustomMetadata = override.CustomMetadata
	instance.Sources = uniqueSorted(instance.Sources)
	sort.Slice(instance.IPv4Networks, func(i, j int) bool {
		return instance.IPv4Networks[i].Address < instance.IPv4Networks[j].Address
	})
	if len(instance.IPv4Networks) > 0 {
		instance.PublicIPv4 = instance.IPv4Networks[0].Address
		instance.GatewayIPv4 = instance.IPv4Networks[0].Gateway
		if ip := net.ParseIP(instance.PublicIPv4); ip != nil {
			if matched := subnetFor(ip, gateways); matched != nil {
				instance.Kea = map[string]any{"subnet_id": matched.id, "subnet": matched.network.String(), "gateway": matched.gateway, "next_server": matched.nextServer, "options": matched.options}
			}
		}
	}
	return instance, len(instance.Sources) > 0
}

func proxmoxMetadata(r Resource, config, status map[string]any) map[string]any {
	result := map[string]any{"node": r.Node, "type": r.Type, "status": r.Status, "pool": r.Pool, "tags": splitTags(r.Tags), "uptime_seconds": r.Uptime, "max_cpu": r.MaxCPU, "max_memory_bytes": r.MaxMem, "template": r.Template != 0}
	allowed := []string{"arch", "ostype", "bios", "machine", "cpu", "cores", "sockets", "memory", "balloon", "agent", "onboot", "startup", "protection", "vmgenid", "nameserver", "searchdomain", "hostname", "ha", "lock", "qmpstatus"}
	for _, key := range allowed {
		if value, ok := config[key]; ok { result[key] = value }
		if value, ok := status[key]; ok { result[key] = value }
	}
	return result
}

func splitTags(value string) []string {
	if value == "" { return []string{} }
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ';' || r == ',' })
	sort.Strings(parts)
	return parts
}

func parseNetworkInterfaces(config map[string]any) []NetworkInterface {
	result := []NetworkInterface{}
	for i := 0; i < 32; i++ {
		name := "net" + strconv.Itoa(i)
		raw, ok := config[name].(string)
		if !ok || raw == "" { continue }
		item := NetworkInterface{Name: name}
		parts := strings.Split(raw, ",")
		if len(parts) > 0 {
			first := strings.SplitN(parts[0], "=", 2)
			if len(first) == 2 {
				if first[0] == "name" { item.Model = "veth" } else { item.Model, item.MAC = first[0], first[1] }
			}
		}
		for _, part := range parts[1:] {
			pair := strings.SplitN(part, "=", 2)
			if len(pair) != 2 { continue }
			switch pair[0] { case "bridge": item.Bridge = pair[1]; case "tag": item.VLAN = pair[1]; case "firewall": item.Firewall = pair[1]; case "mtu": item.MTU = pair[1]; case "rate": item.Rate = pair[1]; case "hwaddr": item.MAC = pair[1]; case "name": if item.Model == "" { item.Model = "veth" } }
		}
		result = append(result, item)
	}
	return result
}

func parseIPOrCIDR(value string) (net.IP, *net.IPNet, error) {
	if strings.Contains(value, "/") {
		ip, network, err := net.ParseCIDR(value)
		return ip, network, err
	}
	ip := net.ParseIP(value)
	if ip == nil { return nil, nil, fmt.Errorf("invalid IP") }
	bits := 128
	if ip.To4() != nil { bits = 32 }
	_, network, err := net.ParseCIDR(ip.String() + "/" + strconv.Itoa(bits))
	return ip, network, err
}

func isTrue(value any) bool {
	switch v := value.(type) {
	case bool: return v
	case float64: return v != 0
	case string: return v == "1" || strings.EqualFold(v, "true")
	default: return false
	}
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] { seen[value] = true; result = append(result, value) }
	}
	sort.Strings(result)
	return result
}

func loadKeaGateways(path string) ([]subnetGateway, error) {
	b, err := os.ReadFile(path)
	if err != nil { return nil, err }
	b = stripJSONComments(b)
	var root any
	if err := json.Unmarshal(b, &root); err != nil { return nil, fmt.Errorf("parse Kea configuration: %w", err) }
	var result []subnetGateway
	walkKea(root, &result)
	if len(result) == 0 { return nil, fmt.Errorf("no IPv4 subnet router options found in %s", path) }
	return result, nil
}

func walkKea(value any, result *[]subnetGateway) {
	switch current := value.(type) {
	case map[string]any:
		if subnet, ok := current["subnet"].(string); ok {
			if _, network, err := net.ParseCIDR(subnet); err == nil && network.IP.To4() != nil {
				candidate := subnetGateway{network: network, options: make(map[string]string)}
				if id, ok := current["id"].(float64); ok { candidate.id = int(id) }
				if nextServer, ok := current["next-server"].(string); ok { candidate.nextServer = nextServer }
				if options, ok := current["option-data"].([]any); ok {
					for _, raw := range options {
						option, _ := raw.(map[string]any)
						name, _ := option["name"].(string)
						data, _ := option["data"].(string)
						if name != "" && data != "" { candidate.options[name] = data }
						if name == "routers" {
							gateway := strings.TrimSpace(strings.Split(data, ",")[0])
							if net.ParseIP(gateway) != nil { candidate.gateway = gateway }
						}
					}
				}
				if candidate.gateway != "" { *result = append(*result, candidate) }
			}
		}
		for _, child := range current { walkKea(child, result) }
	case []any:
		for _, child := range current { walkKea(child, result) }
	}
}

func networkFor(ip net.IP, gateways []subnetGateway) (string, string) {
	matched := subnetFor(ip, gateways)
	if matched == nil { return "", "" }
	return matched.network.String(), matched.gateway
}

func subnetFor(ip net.IP, gateways []subnetGateway) *subnetGateway {
	best, bestBits := (*subnetGateway)(nil), -1
	for i := range gateways {
		if gateways[i].network.Contains(ip) {
			ones, _ := gateways[i].network.Mask.Size()
			if ones > bestBits { best, bestBits = &gateways[i], ones }
		}
	}
	return best
}

func stripJSONComments(input []byte) []byte {
	result := make([]byte, 0, len(input))
	inString, escaped, lineComment, blockComment := false, false, false, false
	for i := 0; i < len(input); i++ {
		c := input[i]
		if lineComment { if c == '\n' { lineComment = false; result = append(result, c) }; continue }
		if blockComment { if c == '*' && i+1 < len(input) && input[i+1] == '/' { blockComment = false; i++ }; continue }
		if inString {
			result = append(result, c)
			if escaped { escaped = false } else if c == '\\' { escaped = true } else if c == '"' { inString = false }
			continue
		}
		if c == '"' { inString = true; result = append(result, c); continue }
		if c == '/' && i+1 < len(input) && input[i+1] == '/' { lineComment = true; i++; continue }
		if c == '/' && i+1 < len(input) && input[i+1] == '*' { blockComment = true; i++; continue }
		result = append(result, c)
	}
	return result
}

func writeAtomic(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil { return err }
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil { return err }
	tmp, err := os.CreateTemp(filepath.Dir(path), ".metadata-*.json")
	if err != nil { return err }
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0640); err != nil { tmp.Close(); return err }
	if _, err := tmp.Write(b); err != nil { tmp.Close(); return err }
	if err := tmp.Sync(); err != nil { tmp.Close(); return err }
	if err := tmp.Close(); err != nil { return err }
	return os.Rename(tmpName, path)
}

func fatalIf(err error) {
	if err != nil { fmt.Fprintln(os.Stderr, "error:", err); os.Exit(1) }
}
