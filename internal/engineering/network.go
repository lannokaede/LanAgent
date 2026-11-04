// Package engineering contains bounded infrastructure adapters, not business state.
package engineering

import (
	"net"
	"net/http"
	"time"
)

// Clone rather than mutate DefaultTransport: pools are shared only by explicit clients.
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 5 * time.Second
	t.ResponseHeaderTimeout = 30 * time.Second
	t.IdleConnTimeout = 90 * time.Second
	t.MaxIdleConns = 64
	t.MaxIdleConnsPerHost = 8
	t.MaxConnsPerHost = 16
	return t
}
