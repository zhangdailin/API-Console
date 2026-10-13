package api

// The management API reaches a channel only through the registry in
// internal/provider, and a channel registers itself when its package is linked.
// Production links them in cmd/server; the tests link them here.
import _ "orchids-api/internal/provider/all"
