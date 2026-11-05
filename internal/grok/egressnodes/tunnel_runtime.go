package egressnodes

import (
	"errors"
	"net/url"
)

// errTunnelUnavailable marks share-link nodes while no tunnel runtime serves
// them; they are kept but never routed, so traffic can never fall through to a
// direct connection by accident.
var errTunnelUnavailable = errors.New("tunnel protocols are not available in this build yet")

func tunnelEndpoint(string) (*url.URL, error) { return nil, errTunnelUnavailable }

// tunnelRoutable reports whether a share link can carry traffic now.
func tunnelRoutable(string) error { return errTunnelUnavailable }
