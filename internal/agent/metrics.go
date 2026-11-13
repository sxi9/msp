package agent

import (
	"math"
	"net"
	"os"
	"runtime"
	"sync"
)

// collector samples host metrics. It keeps the previous CPU counters so the
// second and later samples can report a percentage; the first sample reports
// cpu_pct null.
type collector struct {
	prevIdle  uint64
	prevTotal uint64
	mu        sync.Mutex
}

func newCollector() *collector { return &collector{} }

// cpuPct turns an idle/total jiffy delta into a busy percentage with one
// decimal, or nil when there is no delta yet (first sample).
func cpuPct(idleDelta, totalDelta uint64) *float64 {
	if totalDelta == 0 {
		return nil
	}
	if idleDelta > totalDelta {
		idleDelta = totalDelta
	}
	v := pctRound(100 * (1 - float64(idleDelta)/float64(totalDelta)))
	return &v
}

// pctRound rounds to one decimal place.
func pctRound(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return math.Round(x*10) / 10
}

// delta feeds the collector a new idle/total counter pair and returns the
// percentage for the interval since the previous call (nil on the first call
// or when the counters went backwards).
func (c *collector) delta(idle, total uint64) *float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var pct *float64
	if c.prevTotal != 0 && total > c.prevTotal && idle >= c.prevIdle {
		pct = cpuPct(idle-c.prevIdle, total-c.prevTotal)
	}
	c.prevIdle, c.prevTotal = idle, total
	return pct
}

// localIPs lists the addresses of interfaces that are up and not loopback:
// IPv4 plus global IPv6, skipping link-local. It never returns nil so the
// JSON is [] rather than null.
func localIPs() []string {
	out := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			if ip.To4() == nil && !ip.IsGlobalUnicast() {
				continue
			}
			out = append(out, ip.String())
		}
	}
	return out
}

// arch is the protocol arch string: amd64, arm64, 386 or arm (= GOARCH).
func arch() string { return runtime.GOARCH }

// hostname is the OS host name, "unknown" when it cannot be read.
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// bytesToGB converts bytes to GiB with one decimal.
func bytesToGB(b uint64) float64 {
	return pctRound(float64(b) / (1 << 30))
}
