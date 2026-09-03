// Package detached provides a dns.ResponseWriter that captures a reply without
// touching the connection the query arrived on.
package detached

import (
	"net"

	"github.com/miekg/dns"
)

// Writer captures a reply while delegating nothing to the live ResponseWriter,
// so a losing leg of a race can finish after ServeDNS has already returned.
//
// plugin/pkg/nonwriter cannot be used for this: it embeds the real writer, and
// a leg that outlives its request would then touch a connection the server may
// have cancelled and handed to another query. Addresses are snapshotted instead,
// because request.Request.Size reaches through to RemoteAddr to size a reply.
type Writer struct {
	local, remote net.Addr

	// Msg is the captured reply, nil if nothing was written. It is written by
	// the leg's own goroutine and read only after that leg has reported.
	Msg *dns.Msg
}

// New returns a Writer carrying w's addresses. Call it on the goroutine that
// owns w, before launching anything that will use the result.
func New(w dns.ResponseWriter) *Writer {
	return &Writer{local: w.LocalAddr(), remote: w.RemoteAddr()}
}

func (d *Writer) LocalAddr() net.Addr         { return d.local }
func (d *Writer) RemoteAddr() net.Addr        { return d.remote }
func (d *Writer) WriteMsg(m *dns.Msg) error   { d.Msg = m; return nil }
func (d *Writer) Write(b []byte) (int, error) { return len(b), nil }
func (d *Writer) Close() error                { return nil }
func (d *Writer) TsigStatus() error           { return nil }
func (d *Writer) TsigTimersOnly(bool)         {}
func (d *Writer) Hijack()                     {}
