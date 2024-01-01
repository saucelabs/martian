package connmetric

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type InstrumentedConn struct {
	net.Conn
	tracker Tracker

	address  string
	start    time.Time
	bytesIn  *uint64
	bytesOut *uint64
	error    atomic.Pointer[error]

	closeOnce  sync.Once
	closeError error
}

func NewInstrumentedConn(conn net.Conn, tracker Tracker) *InstrumentedConn {
	in := uint64(0)
	out := uint64(0)
	return &InstrumentedConn{
		Conn:     conn,
		tracker:  tracker,
		address:  conn.RemoteAddr().String(),
		start:    time.Now(),
		bytesIn:  &in,
		bytesOut: &out,
	}
}

func (ic *InstrumentedConn) Read(b []byte) (int, error) {
	n, err := ic.Conn.Read(b)
	if err != nil && *ic.error.Load() == nil {
		ic.error.Store(&err)
	}
	atomic.AddUint64(ic.bytesIn, uint64(n))

	return n, err
}

func (ic *InstrumentedConn) Write(b []byte) (int, error) {
	n, err := ic.Conn.Write(b)
	if err != nil && *ic.error.Load() == nil {
		ic.error.Store(&err)
	}
	atomic.AddUint64(ic.bytesOut, uint64(n))

	return n, err
}

func (ic *InstrumentedConn) Close() error {
	ic.closeOnce.Do(ic.close)
	return ic.closeError
}

func (ic *InstrumentedConn) close() {
	dur := time.Since(ic.start)
	ic.closeError = ic.Conn.Close()
	ic.tracker.RecordStats(StatsEntry{
		Address:  ic.address,
		Duration: dur,
		BytesIn:  atomic.LoadUint64(ic.bytesIn),
		BytesOut: atomic.LoadUint64(ic.bytesOut),
		Error:    *ic.error.Load(),
	})
	return
}
