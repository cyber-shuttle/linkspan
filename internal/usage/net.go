// Network counters cover the whole node, so they are read only when the job holds the node alone. Each physical
// link counts once: an InfiniBand port, or an Ethernet interface with a device behind it.
//
//	Link, readNetwork
package usage

import (
	"os"
	"path/filepath"
	"strings"
)

type Link struct {
	Type      string `json:"type"`
	XmitBytes *int64 `json:"xmitBytes,omitempty"`
	RcvBytes  *int64 `json:"rcvBytes,omitempty"`
}

func readNetwork() map[string]Link {
	links := map[string]Link{}
	add := func(name, kind, xmit, rcv string, by int64) {
		links[name] = Link{kind, scale(readInt(xmit, ""), by), scale(readInt(rcv, ""), by)}
	}
	ports, _ := filepath.Glob(sysClass + "/infiniband/*/ports/*")
	for _, p := range ports {
		if b, _ := os.ReadFile(p + "/link_layer"); strings.TrimSpace(string(b)) == "InfiniBand" {
			name := strings.Replace(strings.TrimPrefix(p, sysClass+"/infiniband/"), "/ports/", "/", 1)
			add(name, "infiniband", p+"/counters/port_xmit_data", p+"/counters/port_rcv_data", 4)
		}
	}
	ifaces, _ := filepath.Glob(sysClass + "/net/*")
	for _, n := range ifaces {
		if _, err := os.Stat(n + "/device"); err == nil {
			if t := readInt(n+"/type", ""); t != nil && *t == 1 {
				add(filepath.Base(n), "ethernet", n+"/statistics/tx_bytes", n+"/statistics/rx_bytes", 1)
			}
		}
	}
	return links
}
