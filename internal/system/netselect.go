package system

import (
	"fmt"
	"net"
)

func PreferredOutboundInterface() (net.Interface, net.IP, error) {
	ip := outboundIPv4()
	if ip == nil {
		return net.Interface{}, nil, fmt.Errorf("detect outbound ipv4")
	}
	iface, ok := interfaceForIP(ip)
	if !ok {
		return net.Interface{}, nil, fmt.Errorf("map outbound ip %s to interface", ip.String())
	}
	return iface, ip, nil
}

func outboundIPv4() net.IP {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return nil
	}
	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil
	}
	return addr.IP.To4()
}

func interfaceForIP(ip net.IP) (net.Interface, bool) {
	if ip == nil {
		return net.Interface{}, false
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return net.Interface{}, false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if addrContainsIP(addr, ip) {
				return iface, true
			}
		}
	}
	return net.Interface{}, false
}

func addrContainsIP(addr net.Addr, ip net.IP) bool {
	switch typed := addr.(type) {
	case *net.IPNet:
		return typed.Contains(ip)
	case *net.IPAddr:
		return typed.IP.Equal(ip)
	default:
		return false
	}
}
