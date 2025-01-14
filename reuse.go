package libp2pquic

import (
	"net"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
)

// Constants. Defined as variables to simplify testing.
var (
	garbageCollectInterval = 30 * time.Second
	maxUnusedDuration      = 10 * time.Second
)

type reuseConn struct {
	net.PacketConn

	mutex       sync.Mutex
	refCount    int
	unusedSince time.Time
}

func newReuseConn(conn net.PacketConn) *reuseConn {
	return &reuseConn{PacketConn: conn}
}

func (c *reuseConn) IncreaseCount() {
	c.mutex.Lock()
	c.refCount++
	c.unusedSince = time.Time{}
	c.mutex.Unlock()
}

func (c *reuseConn) DecreaseCount() {
	c.mutex.Lock()
	c.refCount--
	if c.refCount == 0 {
		c.unusedSince = time.Now()
	}
	c.mutex.Unlock()
}

func (c *reuseConn) ShouldGarbageCollect(now time.Time) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return !c.unusedSince.IsZero() && c.unusedSince.Add(maxUnusedDuration).Before(now)
}

type reuse struct {
	mutex sync.Mutex

	garbageCollectorRunning bool

	handle *netlink.Handle // Only set on Linux. nil on other systems.

	unicast map[string] /* IP.String() */ map[int] /* port */ *reuseConn
	// global contains connections that are listening on 0.0.0.0 / ::
	global map[int]*reuseConn
}

func newReuse() (*reuse, error) {
	// On non-Linux systems, this will return ErrNotImplemented.
	handle, err := netlink.NewHandle()
	if err == netlink.ErrNotImplemented {
		handle = nil
	} else if err != nil {
		return nil, err
	}
	return &reuse{
		unicast: make(map[string]map[int]*reuseConn),
		global:  make(map[int]*reuseConn),
		handle:  handle,
	}, nil
}

func (r *reuse) runGarbageCollector() {
	ticker := time.NewTicker(garbageCollectInterval)
	defer ticker.Stop()

	for now := range ticker.C {
		var shouldExit bool
		r.mutex.Lock()
		for key, conn := range r.global {
			if conn.ShouldGarbageCollect(now) {
				conn.Close()
				delete(r.global, key)
			}
		}
		for ukey, conns := range r.unicast {
			for key, conn := range conns {
				if conn.ShouldGarbageCollect(now) {
					conn.Close()
					delete(conns, key)
				}
			}
			if len(conns) == 0 {
				delete(r.unicast, ukey)
			}
		}

		// stop the garbage collector if we're not tracking any connections
		if len(r.global) == 0 && len(r.unicast) == 0 {
			r.garbageCollectorRunning = false
			shouldExit = true
		}
		r.mutex.Unlock()

		if shouldExit {
			return
		}
	}
}

// must be called while holding the mutex
func (r *reuse) maybeStartGarbageCollector() {
	if !r.garbageCollectorRunning {
		r.garbageCollectorRunning = true
		go r.runGarbageCollector()
	}
}

// Get the source IP that the kernel would use for dialing.
// This only works on Linux.
// On other systems, this returns an empty slice of IP addresses.
func (r *reuse) getSourceIPs(network string, raddr *net.UDPAddr) ([]net.IP, error) {
	if r.handle == nil {
		return nil, nil
	}

	routes, err := r.handle.RouteGet(raddr.IP)
	if err != nil {
		return nil, err
	}

	ips := make([]net.IP, 0, len(routes))
	for _, route := range routes {
		ips = append(ips, route.Src)
	}
	return ips, nil
}

func (r *reuse) Dial(network string, raddr *net.UDPAddr) (*reuseConn, error) {
	ips, err := r.getSourceIPs(network, raddr)
	if err != nil {
		return nil, err
	}

	r.mutex.Lock()
	defer r.mutex.Unlock()

	conn, err := r.dialLocked(network, raddr, ips)
	if err != nil {
		return nil, err
	}
	conn.IncreaseCount()
	r.maybeStartGarbageCollector()
	return conn, nil
}

func (r *reuse) dialLocked(network string, raddr *net.UDPAddr, ips []net.IP) (*reuseConn, error) {
	for _, ip := range ips {
		// We already have at least one suitable connection...
		if conns, ok := r.unicast[ip.String()]; ok {
			// ... we don't care which port we're dialing from. Just use the first.
			for _, c := range conns {
				return c, nil
			}
		}
	}

	// Use a connection listening on 0.0.0.0 (or ::).
	// Again, we don't care about the port number.
	for _, conn := range r.global {
		return conn, nil
	}

	// We don't have a connection that we can use for dialing.
	// Dial a new connection from a random port.
	var addr *net.UDPAddr
	switch network {
	case "udp4":
		addr = &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	case "udp6":
		addr = &net.UDPAddr{IP: net.IPv6zero, Port: 0}
	}
	conn, err := net.ListenUDP(network, addr)
	if err != nil {
		return nil, err
	}
	rconn := newReuseConn(conn)
	r.global[conn.LocalAddr().(*net.UDPAddr).Port] = rconn
	return rconn, nil
}

func (r *reuse) Listen(network string, laddr *net.UDPAddr) (*reuseConn, error) {
	conn, err := net.ListenUDP(network, laddr)
	if err != nil {
		return nil, err
	}
	localAddr := conn.LocalAddr().(*net.UDPAddr)

	rconn := newReuseConn(conn)
	rconn.IncreaseCount()

	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.maybeStartGarbageCollector()

	// Deal with listen on a global address
	if localAddr.IP.IsUnspecified() {
		// The kernel already checked that the laddr is not already listen
		// so we need not check here (when we create ListenUDP).
		r.global[localAddr.Port] = rconn
		return rconn, err
	}

	// Deal with listen on a unicast address
	if _, ok := r.unicast[localAddr.IP.String()]; !ok {
		r.unicast[localAddr.IP.String()] = make(map[int]*reuseConn)
	}

	// The kernel already checked that the laddr is not already listen
	// so we need not check here (when we create ListenUDP).
	r.unicast[localAddr.IP.String()][localAddr.Port] = rconn
	return rconn, err
}
