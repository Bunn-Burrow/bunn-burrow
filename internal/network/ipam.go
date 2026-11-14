package network

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
)

// IPAM allocates static IP addresses from a CIDR pool.
type IPAM struct {
	mu     sync.Mutex
	net    *net.IPNet
	used   map[string]bool
	nextIP net.IP
}

// NewIPAM creates an IPAM allocator for the given CIDR. The network and
// broadcast addresses are never allocated.
func NewIPAM(cidr string) (*IPAM, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse CIDR %q: %w", cidr, err)
	}
	ipv4 := ipNet.IP.To4()
	if ipv4 == nil {
		return nil, fmt.Errorf("CIDR %q is not an IPv4 network", cidr)
	}
	ipNet.IP = append(net.IP(nil), ipv4...)

	// Start allocation at the first usable host address.
	next := make(net.IP, len(ipNet.IP))
	copy(next, ipNet.IP)
	next[len(next)-1]++

	return &IPAM{
		net:    ipNet,
		used:   make(map[string]bool),
		nextIP: next,
	}, nil
}

// Reserve marks an address as unavailable for allocation.
func (ipam *IPAM) Reserve(ip string) error {
	address := net.ParseIP(ip).To4()
	if address == nil {
		return fmt.Errorf("reserved address %q is not IPv4", ip)
	}
	if !ipam.net.Contains(address) {
		return fmt.Errorf("reserved address %s is outside pool %s", address, ipam.net)
	}
	if address.Equal(ipam.net.IP) || address.Equal(ipam.broadcast()) {
		return fmt.Errorf("reserved address %s is a network or broadcast address", address)
	}

	ipam.mu.Lock()
	defer ipam.mu.Unlock()
	ipam.used[address.String()] = true
	return nil
}

// Allocate returns the next unused IP from the pool as a string. Returning a
// string (rather than net.IP, which is a slice) avoids any backing-array aliasing
// issues between successive allocations.
func (ipam *IPAM) Allocate() (string, error) {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()

	ip := ipam.nextIP
	broadcast := ipam.broadcast()
	for ipam.net.Contains(ip) && !ip.Equal(broadcast) {
		key := ip.String()
		if !ipam.used[key] {
			ipam.used[key] = true
			ipam.nextIP = ipam.nextIPTo(ip)
			return key, nil
		}
		ip = ipam.nextIPTo(ip)
	}
	return "", fmt.Errorf("IPAM pool %s exhausted", ipam.net.String())
}

// Network returns the CIDR network this IPAM allocates from.
func (ipam *IPAM) Network() *net.IPNet {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()
	return &net.IPNet{
		IP:   append(net.IP(nil), ipam.net.IP...),
		Mask: append(net.IPMask(nil), ipam.net.Mask...),
	}
}

// Release frees a previously allocated IP. If the released IP is lower than the
// next allocation pointer, the pointer is rewound so the IP can be reused.
func (ipam *IPAM) Release(ip string) {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()
	delete(ipam.used, ip)

	// Rewind the allocation pointer if the released IP is before it, so it
	// can be reallocated on the next Allocate call.
	released := net.ParseIP(ip).To4()
	if released == nil {
		return
	}
	next := ipam.nextIP.To4()
	if next == nil {
		return
	}
	rel := binary.BigEndian.Uint32(released)
	nxt := binary.BigEndian.Uint32(next)
	if rel < nxt {
		ipam.nextIP = released
	}
}

// nextIPTo returns the next IP address after ip.
func (ipam *IPAM) nextIPTo(ip net.IP) net.IP {
	next := make(net.IP, len(ip))
	copy(next, ip)
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] != 0 {
			break
		}
	}
	return next
}

func (ipam *IPAM) broadcast() net.IP {
	broadcast := make(net.IP, net.IPv4len)
	network := ipam.net.IP.To4()
	for i := range broadcast {
		broadcast[i] = network[i] | ^ipam.net.Mask[i]
	}
	return broadcast
}
