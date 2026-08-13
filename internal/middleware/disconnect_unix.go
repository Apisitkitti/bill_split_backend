//go:build unix

package middleware

import (
	"context"
	"net"
	"syscall"
	"time"
)

// disconnectPoll is how often an in-flight request checks whether its client is
// still there. Most requests finish inside one tick and never poll at all; the
// ones that matter are the slow ones, and a quarter second of extra work on a
// query that was going to run for seconds is not worth a finer grain.
const disconnectPoll = 250 * time.Millisecond

// watchDisconnect cancels the request when the client goes away, and returns the
// function that stops watching.
//
// This exists because fasthttp cannot tell us. RequestCtx.Done() is documented
// as closing only on server shutdown — creating a channel per request was judged
// too expensive — and the connection's read loop is parked inside our handler,
// so nothing notices the FIN until the handler returns. Without this, a member
// who closes the LIFF webview leaves their query running to completion on a
// pooled connection that nobody will ever read the answer from, which on a pool
// of ten is capacity taken from members who *are* still waiting.
//
// The check is a MSG_PEEK recv: it reports an orderly close as zero bytes and a
// reset as an error, while data from a client that is still there stays in the
// socket buffer untouched. A plain Read would have to consume that byte, and a
// pipelined request would be silently eaten.
//
// It fails open in every direction. A connection that cannot be peeked at — a
// TLS wrapper, fiber's in-memory test listener — is simply not watched, because
// wrongly deciding a live client has gone would cancel a request that was about
// to succeed.
func watchDisconnect(conn net.Conn, cancel context.CancelFunc) (stop func()) {
	noop := func() {}
	if conn == nil {
		return noop
	}
	sc, ok := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return noop
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return noop
	}

	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(disconnectPoll)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if peerGone(raw) {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// peerGone reports whether the other end of the socket has closed or reset it.
func peerGone(raw syscall.RawConn) bool {
	var gone bool
	buf := make([]byte, 1)

	// The callback returns true unconditionally: this is a poll, not a wait, and
	// returning false would park the goroutine on the socket becoming readable —
	// which for a healthy idle client is never.
	err := raw.Read(func(fd uintptr) bool {
		n, _, err := syscall.Recvfrom(int(fd), buf, syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		switch {
		case err == syscall.EAGAIN || err == syscall.EWOULDBLOCK:
			gone = false // connected, nothing said yet
		case err != nil:
			gone = true // reset, or a socket we can no longer reason about
		default:
			// A zero-length read is the orderly close. Anything longer is a
			// client that is still there and has sent more; it was peeked at,
			// so it is still queued for whoever reads next.
			gone = n == 0
		}
		return true
	})
	if err != nil {
		return false
	}
	return gone
}
