package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestKeaParsingAndLongestPrefixMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "kea.json")
	data := `{
  // Kea permits comments.
  "Dhcp4": {
    "subnet4": [
      {
        "id": 1,
        "subnet": "192.0.2.0/24",
        "option-data": [{"name":"routers","data":"192.0.2.1"}]
      },
      {
        "id": 2,
        "subnet": "192.0.2.40/29",
        "next-server": "192.0.2.42",
        "option-data": [
          {"name":"routers","data":"192.0.2.41"},
          {"name":"domain-name-servers","data":"203.0.113.53"}
        ],
        "reservations": [
          {
            "hw-address": "02:00:00:00:00:43",
            "ip-address": "192.0.2.43",
            "user-context": {"proxmox-vmid": 101, "proxmox-primary": true}
          },
          {
            "hw-address": "02:00:00:00:00:44",
            "ip-address": "192.0.2.44",
            "user-context": {"proxmox-vmid": "101"}
          }
        ]
      }
    ]
  }
}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	kea, err := loadKeaData(path)
	if err != nil {
		t.Fatal(err)
	}
	matched := subnetFor(net.ParseIP("192.0.2.43"), kea.gateways)
	if matched == nil {
		t.Fatal("no subnet matched")
	}
	if matched.id != 2 || matched.network.String() != "192.0.2.40/29" || matched.gateway != "192.0.2.41" || matched.nextServer != "192.0.2.42" {
		t.Fatalf("unexpected match: %+v", matched)
	}
	if matched.options["domain-name-servers"] != "203.0.113.53" {
		t.Fatalf("subnet options were not preserved: %+v", matched.options)
	}
	reservations := kea.reservations[101]
	if len(reservations) != 2 {
		t.Fatalf("got %d tagged reservations: %+v", len(reservations), reservations)
	}
	if reservations[0].Address != "192.0.2.43" || reservations[0].HWAddress != "02:00:00:00:00:43" || !reservations[0].Primary {
		t.Fatalf("unexpected primary reservation: %+v", reservations[0])
	}
	if reservations[1].Address != "192.0.2.44" || reservations[1].HWAddress != "02:00:00:00:00:44" || reservations[1].Primary {
		t.Fatalf("unexpected secondary reservation: %+v", reservations[1])
	}
}

func TestParseQEMUNetworkInterface(t *testing.T) {
	t.Parallel()
	interfaces := parseNetworkInterfaces(map[string]any{
		"net0": "virtio=02:00:00:00:00:01,bridge=vmbr0,firewall=1,tag=42,mtu=1450,rate=100",
	})
	if len(interfaces) != 1 {
		t.Fatalf("got %d interfaces", len(interfaces))
	}
	got := interfaces[0]
	if got.Model != "virtio" || got.MAC != "02:00:00:00:00:01" || got.Bridge != "vmbr0" || got.VLAN != "42" || got.MTU != "1450" || got.Rate != "100" {
		t.Fatalf("unexpected interface: %+v", got)
	}
}

func TestTaggedReservationSelectsPrimaryAndMergesIPSet(t *testing.T) {
	t.Parallel()
	_, network, err := net.ParseCIDR("192.0.2.40/29")
	if err != nil {
		t.Fatal(err)
	}
	instance, ok := makeInstance(
		Resource{VMID: 101, Type: "qemu", Name: "vm1"},
		[]IPSetEntry{{CIDR: "192.0.2.43/32"}},
		[]subnetGateway{{network: network, gateway: "192.0.2.41"}},
		[]keaReservation{
			{VMID: 101, Address: "192.0.2.43", HWAddress: "02:00:00:00:00:43"},
			{VMID: 101, Address: "192.0.2.44", HWAddress: "02:00:00:00:00:44", Primary: true},
		},
		map[string]any{}, map[string]any{}, SyncConfig{},
	)
	if !ok {
		t.Fatal("instance was not created")
	}
	if instance.PublicIPv4 != "192.0.2.44" || instance.GatewayIPv4 != "192.0.2.41" {
		t.Fatalf("unexpected primary: address=%q gateway=%q", instance.PublicIPv4, instance.GatewayIPv4)
	}
	if len(instance.IPv4Networks) != 2 || len(instance.Sources) != 2 {
		t.Fatalf("addresses were not merged: networks=%+v sources=%+v", instance.IPv4Networks, instance.Sources)
	}
	if instance.IPv4Networks[0].DHCPMAC != "02:00:00:00:00:43" || instance.IPv4Networks[0].Primary {
		t.Fatalf("unexpected secondary record: %+v", instance.IPv4Networks[0])
	}
	if !instance.IPv4Networks[1].Primary {
		t.Fatalf("tagged primary was not retained: %+v", instance.IPv4Networks[1])
	}
}

func TestCommentStripPreservesStrings(t *testing.T) {
	t.Parallel()
	input := []byte(`{"url":"http://example.invalid/a/*b*/","value":1}// comment`)
	got := stripJSONComments(input)
	if string(got) != `{"url":"http://example.invalid/a/*b*/","value":1}` {
		t.Fatalf("unexpected output: %s", got)
	}
}

func TestDuplicateSourceOwnershipIsRejected(t *testing.T) {
	t.Parallel()
	err := validateUniqueSources([]WebInstance{
		{VMID: 101, Sources: []string{"192.0.2.43/32"}},
		{VMID: 102, Sources: []string{"192.0.2.43/32"}},
	})
	if err == nil {
		t.Fatal("duplicate source ownership was accepted")
	}
}
