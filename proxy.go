// Copyright 2015 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package martian

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/martian/v3/dialvia"
	"github.com/google/martian/v3/log"
	"github.com/google/martian/v3/mitm"
	"github.com/google/martian/v3/nosigpipe"
	"github.com/google/martian/v3/proxyutil"
	"github.com/google/martian/v3/trafficshape"
	"golang.org/x/net/http/httpguts"
)

var errClose = errors.New("closing connection")
var noop = Noop("martian")

func isCloseable(err error) bool {
	if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
		return true
	}

	switch err {
	case io.EOF, io.ErrClosedPipe, errClose:
		return true
	}

	if strings.Contains(err.Error(), "tls:") {
		return true
	}

	return false
}

// Proxy is an HTTP proxy with support for TLS MITM and customizable behavior.
type Proxy struct {
	// AllowHTTP disables automatic HTTP to HTTPS upgrades when the listener is TLS.
	AllowHTTP bool

	// ConnectPassthrough passes CONNECT requests to the RoundTripper,
	// and uses the response body as the connection.
	ConnectPassthrough bool

	// WithoutWarning disables the warning header added to requests and responses when modifier errors occur.
	WithoutWarning bool

	// ErrorResponse specifies a custom error HTTP response to send when a proxying error occurs.
	ErrorResponse func(req *http.Request, err error) *http.Response

	// ReadTimeout is the maximum duration for reading the entire
	// request, including the body. A zero or negative value means
	// there will be no timeout.
	//
	// Because ReadTimeout does not let Handlers make per-request
	// decisions on each request body's acceptable deadline or
	// upload rate, most users will prefer to use
	// ReadHeaderTimeout. It is valid to use them both.
	ReadTimeout time.Duration

	// ReadHeaderTimeout is the amount of time allowed to read
	// request headers. The connection's read deadline is reset
	// after reading the headers and the Handler can decide what
	// is considered too slow for the body. If ReadHeaderTimeout
	// is zero, the value of ReadTimeout is used. If both are
	// zero, there is no timeout.
	ReadHeaderTimeout time.Duration

	// WriteTimeout is the maximum duration before timing out
	// writes of the response. It is reset whenever a new
	// request's header is read. Like ReadTimeout, it does not
	// let Handlers make decisions on a per-request basis.
	// A zero or negative value means there will be no timeout.
	WriteTimeout time.Duration

	// CloseAfterReply closes the connection after the response has been sent.
	CloseAfterReply bool

	roundTripper http.RoundTripper
	dial         func(context.Context, string, string) (net.Conn, error)
	mitm         *mitm.Config
	proxyURL     func(*http.Request) (*url.URL, error)
	conns        sync.WaitGroup
	connsMu      sync.Mutex // protects conns.Add/Wait from concurrent access
	closing      chan bool

	reqmod RequestModifier
	resmod ResponseModifier
}

// NewProxy returns a new HTTP proxy.
func NewProxy() *Proxy {
	proxy := &Proxy{
		roundTripper: &http.Transport{
			// TODO(adamtanner): This forces the http.Transport to not upgrade requests
			// to HTTP/2 in Go 1.6+. Remove this once Martian can support HTTP/2.
			TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		closing: make(chan bool),
		reqmod:  noop,
		resmod:  noop,
	}
	proxy.SetDialContext((&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext)
	return proxy
}

// GetRoundTripper gets the http.RoundTripper of the proxy.
func (p *Proxy) GetRoundTripper() http.RoundTripper {
	return p.roundTripper
}

// SetRoundTripper sets the http.RoundTripper of the proxy.
func (p *Proxy) SetRoundTripper(rt http.RoundTripper) {
	p.roundTripper = rt

	if tr, ok := p.roundTripper.(*http.Transport); ok {
		tr.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		tr.Proxy = p.proxyURL
		tr.DialContext = p.dial
	}
}

// SetUpstreamProxy sets the proxy that receives requests from this proxy.
func (p *Proxy) SetUpstreamProxy(proxyURL *url.URL) {
	p.SetUpstreamProxyFunc(http.ProxyURL(proxyURL))
}

// SetUpstreamProxyFunc sets proxy function as in http.Transport.Proxy.
func (p *Proxy) SetUpstreamProxyFunc(f func(*http.Request) (*url.URL, error)) {
	p.proxyURL = f

	if tr, ok := p.roundTripper.(*http.Transport); ok {
		tr.Proxy = f
	}
}

// SetMITM sets the config to use for MITMing of CONNECT requests.
func (p *Proxy) SetMITM(config *mitm.Config) {
	p.mitm = config
}

// SetDialContext sets the dial func used to establish a connection.
func (p *Proxy) SetDialContext(dial func(context.Context, string, string) (net.Conn, error)) {
	p.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, e := dial(ctx, network, addr)
		nosigpipe.IgnoreSIGPIPE(c)
		return c, e
	}

	if tr, ok := p.roundTripper.(*http.Transport); ok {
		tr.DialContext = p.dial
	}
}

// Close sets the proxy to the closing state so it stops receiving new connections,
// finishes processing any inflight requests, and closes existing connections without
// reading anymore requests from them.
func (p *Proxy) Close() {
	log.Infof("martian: closing down proxy")

	close(p.closing)

	log.Infof("martian: waiting for connections to close")
	p.connsMu.Lock()
	p.conns.Wait()
	p.connsMu.Unlock()
	log.Infof("martian: all connections closed")
}

// Closing returns whether the proxy is in the closing state.
func (p *Proxy) Closing() bool {
	select {
	case <-p.closing:
		return true
	default:
		return false
	}
}

// SetRequestModifier sets the request modifier.
func (p *Proxy) SetRequestModifier(reqmod RequestModifier) {
	if reqmod == nil {
		reqmod = noop
	}

	p.reqmod = reqmod
}

// SetResponseModifier sets the response modifier.
func (p *Proxy) SetResponseModifier(resmod ResponseModifier) {
	if resmod == nil {
		resmod = noop
	}

	p.resmod = resmod
}

// Serve accepts connections from the listener and handles the requests.
func (p *Proxy) Serve(l net.Listener) error {
	defer l.Close()

	var delay time.Duration
	for {
		if p.Closing() {
			return nil
		}

		conn, err := l.Accept()
		nosigpipe.IgnoreSIGPIPE(conn)
		if err != nil {
			if nerr, ok := err.(net.Error); ok && nerr.Temporary() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else {
					delay *= 2
				}
				if max := time.Second; delay > max {
					delay = max
				}

				log.Debugf("martian: temporary error on accept: %v", err)
				time.Sleep(delay)
				continue
			}

			if errors.Is(err, net.ErrClosed) {
				log.Debugf("martian: listener closed, returning")
				return err
			}

			log.Errorf("martian: failed to accept: %v", err)
			return err
		}
		delay = 0
		log.Debugf("martian: accepted connection from %s", conn.RemoteAddr())

		if tconn, ok := conn.(*net.TCPConn); ok {
			tconn.SetKeepAlive(true)
			tconn.SetKeepAlivePeriod(3 * time.Minute)
		}

		go p.handleLoop(conn)
	}
}

func (p *Proxy) handleLoop(conn net.Conn) {
	p.connsMu.Lock()
	p.conns.Add(1)
	p.connsMu.Unlock()
	defer p.conns.Done()
	defer conn.Close()
	if p.Closing() {
		return
	}

	var (
		brw = bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		s   = newSession(conn, brw)
		ctx = withSession(s)
	)

	const maxConsecutiveErrors = 5
	errors := 0
	for {
		if err := p.handle(ctx, conn, brw); err != nil {
			if isCloseable(err) {
				log.Debugf("martian: closing connection: %v", conn.RemoteAddr())
				return
			}

			errors++
			if errors >= maxConsecutiveErrors {
				log.Errorf("martian: closing connection after %d consecutive errors: %v", errors, err)
				return
			}
		} else {
			errors = 0
		}

		if s.Hijacked() {
			log.Debugf("martian: closing connection: %v", conn.RemoteAddr())
			return
		}
	}
}

func (p *Proxy) readHeaderTimeout() time.Duration {
	if p.ReadHeaderTimeout > 0 {
		return p.ReadHeaderTimeout
	}
	return p.ReadTimeout
}

func (p *Proxy) readRequest(ctx *Context, conn net.Conn, brw *bufio.ReadWriter) (req *http.Request, err error) {
	var (
		wholeReqDeadline time.Time // or zero if none
		hdrDeadline      time.Time // or zero if none
	)
	t0 := time.Now()
	if d := p.readHeaderTimeout(); d > 0 {
		hdrDeadline = t0.Add(d)
	}
	if d := p.ReadTimeout; d > 0 {
		wholeReqDeadline = t0.Add(d)
	}

	if deadlineErr := conn.SetReadDeadline(hdrDeadline); deadlineErr != nil {
		log.Errorf("martian: can't set read header deadline: %v", deadlineErr)
	}

	req, err = http.ReadRequest(brw.Reader)
	if err != nil {
		if isCloseable(err) {
			log.Debugf("martian: connection closed prematurely: %v", err)
		} else {
			log.Errorf("martian: failed to read request: %v", err)
		}
		if cw, ok := asCloseWriter(conn); ok {
			cw.CloseWrite()
		}
	} else {
		select {
		case <-p.closing:
			err = errClose
		default:
		}

		req = req.WithContext(ctx.addToContext(req.Context()))
	}

	// Adjust the read deadline if necessary.
	if !hdrDeadline.Equal(wholeReqDeadline) {
		if deadlineErr := conn.SetReadDeadline(wholeReqDeadline); deadlineErr != nil {
			log.Errorf("martian: can't set read deadline: %v", deadlineErr)
		}
	}

	return
}

func (p *Proxy) handleConnectRequest(ctx *Context, req *http.Request, session *Session, brw *bufio.ReadWriter, conn net.Conn) error {
	if err := p.reqmod.ModifyRequest(req); err != nil {
		log.Errorf("martian: error modifying CONNECT request: %v", err)
		p.warning(req.Header, err)
	}
	if session.Hijacked() {
		log.Debugf("martian: connection hijacked by request modifier")
		return nil
	}

	if p.mitm != nil {
		log.Debugf("martian: attempting MITM for connection: %s / %s", req.Host, req.URL.String())

		res := proxyutil.NewResponse(200, nil, req)

		if err := p.resmod.ModifyResponse(res); err != nil {
			log.Errorf("martian: error modifying CONNECT response: %v", err)
			p.warning(res.Header, err)
		}
		if session.Hijacked() {
			log.Debugf("martian: connection hijacked by response modifier")
			return nil
		}

		if err := res.Write(brw); err != nil {
			log.Errorf("martian: got error while writing response back to client: %v", err)
		}
		if err := brw.Flush(); err != nil {
			log.Errorf("martian: got error while flushing response back to client: %v", err)
		}

		log.Debugf("martian: completed MITM for connection: %s", req.Host)

		b := make([]byte, 1)
		if _, err := brw.Read(b); err != nil {
			log.Errorf("martian: error peeking message through CONNECT tunnel to determine type: %v", err)
		}

		// Drain all of the rest of the buffered data.
		buf := make([]byte, brw.Reader.Buffered())
		brw.Read(buf)

		// 22 is the TLS handshake.
		// https://tools.ietf.org/html/rfc5246#section-6.2.1
		if b[0] == 22 {
			// Prepend the previously read data to be read again by
			// http.ReadRequest.
			tlsconn := tls.Server(&peekedConn{conn, io.MultiReader(bytes.NewReader(b), bytes.NewReader(buf), conn)}, p.mitm.TLSForHost(req.Host))

			if err := tlsconn.Handshake(); err != nil {
				p.mitm.HandshakeErrorCallback(req, err)
				return err
			}
			if tlsconn.ConnectionState().NegotiatedProtocol == "h2" {
				return p.mitm.H2Config().Proxy(p.closing, tlsconn, req.URL)
			}

			var nconn net.Conn
			nconn = tlsconn
			// If the original connection is a traffic shaped connection, wrap the tls
			// connection inside a traffic shaped connection too.
			if ptsconn, ok := conn.(*trafficshape.Conn); ok {
				nconn = ptsconn.Listener.GetTrafficShapedConn(tlsconn)
			}
			brw.Writer.Reset(nconn)
			brw.Reader.Reset(nconn)
			return p.handle(ctx, nconn, brw)
		}

		// Prepend the previously read data to be read again by http.ReadRequest.
		brw.Reader.Reset(io.MultiReader(bytes.NewReader(b), bytes.NewReader(buf), conn))
		return p.handle(ctx, conn, brw)
	}

	log.Debugf("martian: attempting to establish CONNECT tunnel: %s", req.URL.Host)
	var (
		res  *http.Response
		cr   io.Reader
		cw   io.WriteCloser
		cerr error
	)
	if p.ConnectPassthrough {
		pr, pw := io.Pipe()
		req.Body = pr
		defer req.Body.Close()

		// perform the HTTP roundtrip
		res, cerr = p.roundTrip(ctx, req)
		if res != nil {
			cr = res.Body
			cw = pw

			if res.StatusCode/100 == 2 {
				res = proxyutil.NewResponse(200, nil, req)
			}
		}
	} else {
		var cconn net.Conn
		res, cconn, cerr = p.connect(req)

		if cconn != nil {
			defer cconn.Close()
			cr = cconn
			cw = cconn
		}
	}

	if cerr != nil {
		log.Errorf("martian: failed to CONNECT: %v", cerr)
		res = p.errorResponse(req, cerr)
		p.warning(res.Header, cerr)
	}
	defer res.Body.Close()

	if err := p.resmod.ModifyResponse(res); err != nil {
		log.Errorf("martian: error modifying CONNECT response: %v", err)
		p.warning(res.Header, err)
	}
	if session.Hijacked() {
		log.Debugf("martian: connection hijacked by response modifier")
		return nil
	}

	if res.StatusCode != 200 {
		if cerr == nil {
			log.Errorf("martian: CONNECT rejected with status code: %d", res.StatusCode)
		}
		if err := res.Write(brw); err != nil {
			log.Errorf("martian: got error while writing response back to client: %v", err)
		}
		err := brw.Flush()
		if err != nil {
			log.Errorf("martian: got error while flushing response back to client: %v", err)
		}
		return err
	}

	res.ContentLength = -1

	if err := p.tunnel("CONNECT", res, brw, conn, cw, cr); err != nil {
		log.Errorf("martian: CONNECT tunnel: %w", err)
	}

	return errClose
}

func (p *Proxy) handleUpgradeResponse(res *http.Response, brw *bufio.ReadWriter, conn net.Conn) error {
	resUpType := upgradeType(res.Header)

	uconn, ok := res.Body.(io.ReadWriteCloser)
	if !ok {
		log.Errorf("martian: internal error: switching protocols response with non-writable body")
		return errClose
	}

	res.Body = nil

	if err := p.tunnel(resUpType, res, brw, conn, uconn, uconn); err != nil {
		log.Errorf("martian: %s tunnel: %w", resUpType, err)
	}

	return errClose
}

func (p *Proxy) tunnel(name string, res *http.Response, brw *bufio.ReadWriter, conn net.Conn, cw io.Writer, cr io.Reader) error {
	if err := res.Write(brw); err != nil {
		return fmt.Errorf("got error while writing response back to client: %w", err)
	}
	if err := brw.Flush(); err != nil {
		return fmt.Errorf("got error while flushing response back to client: %w", err)
	}
	if err := drainBuffer(cw, brw.Reader); err != nil {
		return fmt.Errorf("got error while draining read buffer: %w", err)
	}

	donec := make(chan bool, 2)
	go copySync("outbound "+name, cw, conn, donec)
	go copySync("inbound "+name, conn, cr, donec)

	log.Debugf("martian: switched protocols, proxying %s traffic", name)
	<-donec
	<-donec
	log.Debugf("martian: closed %s tunnel", name)

	return nil
}

func drainBuffer(w io.Writer, r *bufio.Reader) error {
	if n := r.Buffered(); n > 0 {
		rbuf, err := r.Peek(n)
		if err != nil {
			return err
		}
		w.Write(rbuf)
	}
	return nil
}

var copyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

func copySync(name string, w io.Writer, r io.Reader, donec chan<- bool) {
	bufp := copyBufPool.Get().(*[]byte)
	buf := *bufp
	defer copyBufPool.Put(bufp)

	if _, err := io.CopyBuffer(w, r, buf); err != nil && err != io.EOF {
		log.Errorf("martian: failed to copy %s tunnel: %v", name, err)
	}
	if cw, ok := asCloseWriter(w); ok {
		cw.CloseWrite()
	} else if pw, ok := w.(*io.PipeWriter); ok {
		pw.Close()
	} else {
		log.Errorf("martian: cannot close write side of %s tunnel (%T)", name, w)
	}

	log.Debugf("martian: %s tunnel finished copying", name)
	donec <- true
}

func (p *Proxy) handle(ctx *Context, conn net.Conn, brw *bufio.ReadWriter) error {
	log.Debugf("martian: waiting for request: %v", conn.RemoteAddr())

	session := ctx.Session()
	ctx = withSession(session)

	req, err := p.readRequest(ctx, conn, brw)
	if err != nil {
		return err
	}
	defer req.Body.Close()

	if tsconn, ok := conn.(*trafficshape.Conn); ok {
		wrconn := tsconn.GetWrappedConn()
		if sconn, ok := wrconn.(*tls.Conn); ok {
			session.MarkSecure()

			cs := sconn.ConnectionState()
			req.TLS = &cs
		}
	}

	if tconn, ok := conn.(*tls.Conn); ok {
		session.MarkSecure()

		cs := tconn.ConnectionState()
		req.TLS = &cs
	}

	req.RemoteAddr = conn.RemoteAddr().String()
	if req.URL.Host == "" {
		req.URL.Host = req.Host
	}

	if req.Method == "CONNECT" {
		return p.handleConnectRequest(ctx, req, session, brw, conn)
	}

	if req.URL.Scheme == "" {
		req.URL.Scheme = "http"
		if session.IsSecure() {
			req.URL.Scheme = "https"
		}
	} else if req.URL.Scheme == "http" {
		if session.IsSecure() && !p.AllowHTTP {
			log.Infof("martian: forcing HTTPS inside secure session")
			req.URL.Scheme = "https"
		}
	}

	reqUpType := upgradeType(req.Header)
	if reqUpType != "" {
		log.Debugf("martian: upgrade request: %s", reqUpType)
	}
	if err := p.reqmod.ModifyRequest(req); err != nil {
		log.Errorf("martian: error modifying request: %v", err)
		p.warning(req.Header, err)
	}
	if session.Hijacked() {
		log.Debugf("martian: connection hijacked by request modifier")
		return nil
	}

	// after stripping all the hop-by-hop connection headers above, add back any
	// necessary for protocol upgrades, such as for websockets.
	if reqUpType != "" {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", reqUpType)
	}

	// perform the HTTP roundtrip
	res, err := p.roundTrip(ctx, req)
	if err != nil {
		log.Errorf("martian: failed to round trip: %v", err)
		res = p.errorResponse(req, err)
		p.warning(res.Header, err)
	}
	defer res.Body.Close()

	// set request to original request manually, res.Request may be changed in transport.
	// see https://github.com/google/martian/issues/298
	res.Request = req

	resUpType := upgradeType(res.Header)
	if resUpType != "" {
		log.Debugf("martian: upgrade response: %s", resUpType)
	}
	if err := p.resmod.ModifyResponse(res); err != nil {
		log.Errorf("martian: error modifying response: %v", err)
		p.warning(res.Header, err)
	}
	if session.Hijacked() {
		log.Debugf("martian: connection hijacked by response modifier")
		return nil
	}

	// after stripping all the hop-by-hop connection headers above, add back any
	// necessary for protocol upgrades, such as for websockets.
	if resUpType != "" {
		res.Header.Set("Connection", "Upgrade")
		res.Header.Set("Upgrade", resUpType)
	}

	var closing error
	if !req.ProtoAtLeast(1, 1) || req.Close || res.Close || p.Closing() {
		log.Debugf("martian: received close request: %v", req.RemoteAddr)
		res.Close = true
		closing = errClose
	}

	// check if conn is a traffic shaped connection.
	if ptsconn, ok := conn.(*trafficshape.Conn); ok {
		ptsconn.Context = &trafficshape.Context{}
		// Check if the request URL matches any URLRegex in Shapes. If so, set the connections's Context
		// with the required information, so that the Write() method of the Conn has access to it.
		for urlregex, buckets := range ptsconn.LocalBuckets {
			if match, _ := regexp.MatchString(urlregex, req.URL.String()); match {
				if rangeStart := proxyutil.GetRangeStart(res); rangeStart > -1 {
					dump, err := httputil.DumpResponse(res, false)
					if err != nil {
						return err
					}
					ptsconn.Context = &trafficshape.Context{
						Shaping:            true,
						Buckets:            buckets,
						GlobalBucket:       ptsconn.GlobalBuckets[urlregex],
						URLRegex:           urlregex,
						RangeStart:         rangeStart,
						ByteOffset:         rangeStart,
						HeaderLen:          int64(len(dump)),
						HeaderBytesWritten: 0,
					}
					// Get the next action to perform, if there.
					ptsconn.Context.NextActionInfo = ptsconn.GetNextActionFromByte(rangeStart)
					// Check if response lies in a throttled byte range.
					ptsconn.Context.ThrottleContext = ptsconn.GetCurrentThrottle(rangeStart)
					if ptsconn.Context.ThrottleContext.ThrottleNow {
						ptsconn.Context.Buckets.WriteBucket.SetCapacity(
							ptsconn.Context.ThrottleContext.Bandwidth)
					}
					log.Infof(
						"trafficshape: Request %s with Range Start: %d matches a Shaping request %s. Enforcing Traffic shaping.",
						req.URL, rangeStart, urlregex)
				}
				break
			}
		}
	}

	// deal with 101 Switching Protocols responses: (WebSocket, h2c, etc)
	if res.StatusCode == 101 {
		return p.handleUpgradeResponse(res, brw, conn)
	}

	if p.WriteTimeout > 0 {
		if deadlineErr := conn.SetWriteDeadline(time.Now().Add(p.WriteTimeout)); deadlineErr != nil {
			log.Errorf("martian: can't set write deadline: %v", deadlineErr)
		}
	}

	// Add support for Server Sent Events - relay HTTP chunks and flush after each chunk.
	// This is safe for events that are smaller than the buffer io.Copy uses (32KB).
	// If the event is larger than the buffer, the event will be split into multiple chunks.
	if shouldFlush(res) {
		err = res.Write(flushAfterChunkWriter{brw.Writer})
	} else {
		err = res.Write(brw)
	}
	if err != nil {
		log.Errorf("martian: got error while writing response back to client: %v", err)
		if _, ok := err.(*trafficshape.ErrForceClose); ok {
			closing = errClose
		}
		if err == io.ErrUnexpectedEOF {
			closing = errClose
		}
	}
	err = brw.Flush()
	if err != nil {
		log.Errorf("martian: got error while flushing response back to client: %v", err)
		if _, ok := err.(*trafficshape.ErrForceClose); ok {
			closing = errClose
		}
	}

	if p.CloseAfterReply {
		closing = errClose
	}
	return closing
}

// A peekedConn subverts the net.Conn.Read implementation, primarily so that
// sniffed bytes can be transparently prepended.
type peekedConn struct {
	net.Conn
	r io.Reader
}

// Read allows control over the embedded net.Conn's read data. By using an
// io.MultiReader one can read from a conn, and then replace what they read, to
// be read again.
func (c *peekedConn) Read(buf []byte) (int, error) { return c.r.Read(buf) }

func (p *Proxy) roundTrip(ctx *Context, req *http.Request) (*http.Response, error) {
	if ctx.SkippingRoundTrip() {
		log.Debugf("martian: skipping round trip")
		return proxyutil.NewResponse(200, nil, req), nil
	}

	return p.roundTripper.RoundTrip(req)
}

func (p *Proxy) warning(h http.Header, err error) {
	if p.WithoutWarning {
		return
	}
	proxyutil.Warning(h, err)
}

func (p *Proxy) errorResponse(req *http.Request, err error) *http.Response {
	if p.ErrorResponse != nil {
		return p.ErrorResponse(req, err)
	}
	return proxyutil.NewResponse(502, nil, req)
}

func (p *Proxy) connect(req *http.Request) (*http.Response, net.Conn, error) {
	var proxyURL *url.URL
	if p.proxyURL != nil {
		u, err := p.proxyURL(req)
		if err != nil {
			return nil, nil, err
		}
		proxyURL = u
	}

	if proxyURL == nil {
		log.Debugf("martian: CONNECT to host directly: %s", req.URL.Host)

		conn, err := p.dial(req.Context(), "tcp", req.URL.Host)
		if err != nil {
			return nil, nil, err
		}

		return proxyutil.NewResponse(200, nil, req), conn, nil
	}

	switch proxyURL.Scheme {
	case "http", "https":
		return p.connectHTTP(req, proxyURL)
	case "socks5":
		return p.connectSOCKS5(req, proxyURL)
	default:
		return nil, nil, fmt.Errorf("martian: unsupported proxy scheme: %s", proxyURL.Scheme)
	}
}

func (p *Proxy) connectHTTP(req *http.Request, proxyURL *url.URL) (res *http.Response, conn net.Conn, err error) {
	log.Debugf("martian: CONNECT with upstream HTTP proxy: %s", proxyURL.Host)

	if proxyURL.Scheme == "https" {
		d := dialvia.HTTPSProxy(p.dial, proxyURL, p.clientTLSConfig())
		res, conn, err = d.DialContextR(req.Context(), "tcp", req.URL.Host)
	} else {
		d := dialvia.HTTPProxy(p.dial, proxyURL)
		res, conn, err = d.DialContextR(req.Context(), "tcp", req.URL.Host)
	}

	if res != nil {
		if res.StatusCode/100 == 2 {
			res.Body.Close()
			return proxyutil.NewResponse(200, nil, req), conn, nil
		}

		// If the proxy returns a non-2xx response, return it to the client.
		// But first, replace the Request with the original request.
		res.Request = req
	}

	return res, conn, err
}

func (p *Proxy) clientTLSConfig() *tls.Config {
	if tr, ok := p.roundTripper.(*http.Transport); ok && tr.TLSClientConfig != nil {
		return tr.TLSClientConfig.Clone()
	}

	return &tls.Config{}
}

func (p *Proxy) connectSOCKS5(req *http.Request, proxyURL *url.URL) (*http.Response, net.Conn, error) {
	log.Debugf("martian: CONNECT with upstream SOCKS5 proxy: %s", proxyURL.Host)

	d := dialvia.SOCKS5Proxy(p.dial, proxyURL)

	conn, err := d.DialContext(req.Context(), "tcp", req.URL.Host)
	if err != nil {
		return nil, nil, err
	}

	return proxyutil.NewResponse(200, nil, req), conn, nil
}

func upgradeType(h http.Header) string {
	if !httpguts.HeaderValuesContainsToken(h["Connection"], "Upgrade") {
		return ""
	}
	return h.Get("Upgrade")
}
