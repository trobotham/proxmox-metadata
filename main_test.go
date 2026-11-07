package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigAndMatchMostSpecificSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{
  "listen": "169.254.169.254:8080",
  "require_token": true,
  "instances": [
    {"sources":["192.0.2.0/24"],"instance_id":"broad","vm_id":100,"hostname":"broad"},
    {"sources":["192.0.2.43/32"],"instance_id":"exact","vm_id":101,"hostname":"exact",
     "ipv4_networks":[{"address":"192.0.2.43","prefix":"192.0.2.40/29","gateway":"192.0.2.41","dhcp_mac":"02:00:00:00:00:43","primary":true}]}
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.listen != "169.254.169.254:8080" || !cfg.requireToken {
		t.Fatalf("unexpected runtime config: %+v", cfg)
	}
	s := &server{cfg: cfg, tokens: make(map[string]tokenRecord)}
	r := httptest.NewRequest("GET", "http://metadata/latest/meta-data/instance-id", nil)
	r.RemoteAddr = "192.0.2.43:54321"
	instance, source, err := s.instanceFor(r)
	if err != nil {
		t.Fatal(err)
	}
	if source != "192.0.2.43" || instance.InstanceID != "exact" {
		t.Fatalf("got source=%q instance=%q", source, instance.InstanceID)
	}
}

func TestTokenIsBoundToSourceAndExpiry(t *testing.T) {
	t.Parallel()
	s := &server{
		cfg: &runtimeConfig{requireToken: true, maxTokenTTL: time.Hour},
		tokens: map[string]tokenRecord{
			"valid": {source: "192.0.2.43", expires: time.Now().Add(time.Minute)},
			"old":   {source: "192.0.2.43", expires: time.Now().Add(-time.Minute)},
		},
	}
	r := httptest.NewRequest("GET", "http://metadata/latest/meta-data/instance-id", nil)
	r.Header.Set(tokenHeader, "valid")
	if !s.authorized(r, "192.0.2.43") {
		t.Fatal("valid token was rejected")
	}
	if s.authorized(r, "198.51.100.20") {
		t.Fatal("token was accepted from a different source")
	}
	r.Header.Set(tokenHeader, "old")
	if s.authorized(r, "192.0.2.43") {
		t.Fatal("expired token was accepted")
	}
}

func TestParseSourceDefaultsToHostPrefix(t *testing.T) {
	t.Parallel()
	v4, err := parseSource("192.0.2.43")
	if err != nil || v4.String() != "192.0.2.43/32" {
		t.Fatalf("IPv4 result=%v error=%v", v4, err)
	}
	v6, err := parseSource("2001:db8:100::43")
	if err != nil || v6.String() != "2001:db8:100::43/128" {
		t.Fatalf("IPv6 result=%v error=%v", v6, err)
	}
}

func TestValidateListenRejectsNonMetadataHosts(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{":80", "0.0.0.0:80", "[::]:80", "127.0.0.1:80", "169.254.169.254", "169.254.169.254:0"} {
		if err := validateListen(bad); err == nil {
			t.Errorf("validateListen(%q) accepted", bad)
		}
	}
	if err := validateListen("169.254.169.254:80"); err != nil {
		t.Fatal(err)
	}
}
