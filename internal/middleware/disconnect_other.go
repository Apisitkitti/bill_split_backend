//go:build !unix

package middleware

import (
	"context"
	"net"
)

// watchDisconnect does nothing off unix: the MSG_PEEK check in
// disconnect_unix.go has no portable equivalent, and guessing wrong would cancel
// live requests. Such a build still gets the request deadline, which bounds the
// damage from an abandoned request to DefaultRequestTimeout rather than to
// however long the query runs.
func watchDisconnect(net.Conn, context.CancelFunc) (stop func()) { return func() {} }
