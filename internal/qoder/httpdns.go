package qoder

// The client resolves the inference host ahead of the request and names the
// address it used in X-Qoder-HTTPDNS-IP, while the URL keeps the host name so
// TLS still presents the right SNI. That is how its httpdns path works: the
// header is an observation of the address chosen, not a routing instruction.
//
// This gateway has no httpdns service, so it resolves the name itself and keeps
// the answer briefly. A resolution failure is not fatal: the request goes out
// without the header, exactly as a client whose resolver has no answer does.

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	inferIPCacheTTL     = 5 * time.Minute
	inferIPLookupBudget = 2 * time.Second
)

type inferIPEntry struct {
	address string
	expires time.Time
}

var inferIPCache sync.Map

// resolvedInferIP returns the address this request's host resolves to, or "".
func resolvedInferIP(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	host := req.URL.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return ""
	}
	if cached, ok := inferIPCache.Load(host); ok {
		if entry, ok := cached.(inferIPEntry); ok && time.Now().Before(entry.expires) {
			return entry.address
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), inferIPLookupBudget)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil || len(addresses) == 0 {
		return ""
	}
	address := strings.TrimSpace(addresses[0])
	inferIPCache.Store(host, inferIPEntry{address: address, expires: time.Now().Add(inferIPCacheTTL)})
	return address
}
