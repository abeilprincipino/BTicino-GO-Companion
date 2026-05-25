package system

import (
	"net"
	"testing"
)

func TestInterfaceForIPNil(t *testing.T) {
	iface, ok := interfaceForIP(nil)
	if ok || iface.Name != "" {
		t.Fatalf("expected no interface for nil ip, got iface=%+v ok=%v", iface, ok)
	}
}

func TestAddrContainsIP(t *testing.T) {
	_, network, err := net.ParseCIDR("10.0.0.172/24")
	if err != nil {
		t.Fatal(err)
	}
	if !addrContainsIP(network, net.ParseIP("10.0.0.172")) {
		t.Fatal("expected network to contain ip")
	}
	if addrContainsIP(network, net.ParseIP("192.168.129.1")) {
		t.Fatal("did not expect network to contain unrelated ip")
	}
}

func TestPreferredOutboundInterfaceCallable(t *testing.T) {
	_, _, _ = PreferredOutboundInterface()
}
