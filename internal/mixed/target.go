package mixed

import "github.com/BFNOC/local-proxy-switcher/internal/upstream"

func targetFromParts(host string, port int) upstream.Target {
	return upstream.Target{Host: host, Port: port}
}
