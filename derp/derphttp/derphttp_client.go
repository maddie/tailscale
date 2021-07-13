// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package derphttp implements DERP-over-HTTP.
//
// This makes DERP look exactly like WebSockets.
// A server can implement DERP over HTTPS and even if the TLS connection
// intercepted using a fake root CA, unless the interceptor knows how to
// detect DERP packets, it will look like a web socket.
package derphttp

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"go4.org/mem"
	"inet.af/netaddr"
	"tailscale.com/derp"
	"tailscale.com/net/dnscache"
	"tailscale.com/net/netns"
	"tailscale.com/net/tlsdial"
	"tailscale.com/net/tshttpproxy"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// Client is a DERP-over-HTTP client.
//
// It automatically reconnects on error retry. That is, a failed Send or
// Recv will report the error and not retry, but subsequent calls to
// Send/Recv will completely re-establish the connection (unless Close
// has been called).
type Client struct {
	TLSConfig *tls.Config        // optional; nil means default
	DNSCache  *dnscache.Resolver // optional; nil means no caching
	MeshKey   string             // optional; for trusted clients
	IsProber  bool               // optional; for probers to optional declare themselves as such

	privateKey key.Private
	logf       logger.Logf

	// Either url or getRegion is non-nil:
	url       *url.URL
	getRegion func() *tailcfg.DERPRegion

	ctx       context.Context // closed via cancelCtx in Client.Close
	cancelCtx context.CancelFunc

	mu           sync.Mutex
	preferred    bool
	canAckPings  bool
	closed       bool
	netConn      io.Closer
	client       *derp.Client
	connGen      int // incremented once per new connection; valid values are >0
	serverPubKey key.Public
}

// NewRegionClient returns a new DERP-over-HTTP client. It connects lazily.
// To trigger a connection, use Connect.
func NewRegionClient(privateKey key.Private, logf logger.Logf, getRegion func() *tailcfg.DERPRegion) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		privateKey: privateKey,
		logf:       logf,
		getRegion:  getRegion,
		ctx:        ctx,
		cancelCtx:  cancel,
	}
	return c
}

// NewNetcheckClient returns a Client that's only able to have its DialRegion method called.
// It's used by the netcheck package.
func NewNetcheckClient(logf logger.Logf) *Client {
	return &Client{logf: logf}
}

// NewClient returns a new DERP-over-HTTP client. It connects lazily.
// To trigger a connection, use Connect.
func NewClient(privateKey key.Private, serverURL string, logf logger.Logf) (*Client, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("derphttp.NewClient: %v", err)
	}
	if urlPort(u) == "" {
		return nil, fmt.Errorf("derphttp.NewClient: invalid URL scheme %q", u.Scheme)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		privateKey: privateKey,
		logf:       logf,
		url:        u,
		ctx:        ctx,
		cancelCtx:  cancel,
	}
	return c, nil
}

// Connect connects or reconnects to the server, unless already connected.
// It returns nil if there was already a good connection, or if one was made.
func (c *Client) Connect(ctx context.Context) error {
	_, _, err := c.connect(ctx, "derphttp.Client.Connect")
	return err
}

// ServerPublicKey returns the server's public key.
//
// It only returns a non-zero value once a connection has succeeded
// from an earlier call.
func (c *Client) ServerPublicKey() key.Public {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverPubKey
}

// SelfPublicKey returns our own public key.
func (c *Client) SelfPublicKey() key.Public {
	return c.privateKey.Public()
}

func urlPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

func (c *Client) targetString(reg *tailcfg.DERPRegion) string {
	if c.url != nil {
		return c.url.String()
	}
	return fmt.Sprintf("region %d (%v)", reg.RegionID, reg.RegionCode)
}

func (c *Client) useHTTPS() bool {
	if c.url != nil && c.url.Scheme == "http" {
		return false
	}
	return true
}

// tlsServerName returns the tls.Config.ServerName value (for the TLS ClientHello).
func (c *Client) tlsServerName(node *tailcfg.DERPNode) string {
	if c.url != nil {
		return c.url.Host
	}
	return node.HostName
}

func (c *Client) urlString(node *tailcfg.DERPNode) string {
	if c.url != nil {
		return c.url.String()
	}
	return fmt.Sprintf("https://%s/derp", node.HostName)
}

func (c *Client) connect(ctx context.Context, caller string) (client *derp.Client, connGen int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, 0, ErrClientClosed
	}
	if c.client != nil {
		return c.client, c.connGen, nil
	}

	// timeout is the fallback maximum time (if ctx doesn't limit
	// it further) to do all of: DNS + TCP + TLS + HTTP Upgrade +
	// DERP upgrade.
	const timeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	go func() {
		select {
		case <-ctx.Done():
			// Either timeout fired (handled below), or
			// we're returning via the defer cancel()
			// below.
		case <-c.ctx.Done():
			// Propagate a Client.Close call into
			// cancelling this context.
			cancel()
		}
	}()
	defer cancel()

	var reg *tailcfg.DERPRegion // nil when using c.url to dial
	if c.getRegion != nil {
		reg = c.getRegion()
		if reg == nil {
			return nil, 0, errors.New("DERP region not available")
		}
	}

	var tcpConn net.Conn

	defer func() {
		if err != nil {
			if ctx.Err() != nil {
				err = fmt.Errorf("%v: %v", ctx.Err(), err)
			}
			err = fmt.Errorf("%s connect to %v: %v", caller, c.targetString(reg), err)
			if tcpConn != nil {
				go tcpConn.Close()
			}
		}
	}()

	var node *tailcfg.DERPNode // nil when using c.url to dial
	if c.url != nil {
		c.logf("%s: connecting to %v", caller, c.url)
		tcpConn, err = c.dialURL(ctx)
	} else {
		c.logf("%s: connecting to derp-%d (%v)", caller, reg.RegionID, reg.RegionCode)
		tcpConn, node, err = c.dialRegion(ctx, reg)
	}
	if err != nil {
		return nil, 0, err
	}

	// Now that we have a TCP connection, force close it if the
	// TLS handshake + DERP setup takes too long.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
			// Normal path. Upgrade occurred in time.
		case <-ctx.Done():
			select {
			case <-done:
				// Normal path. Upgrade occurred in time.
				// But the ctx.Done() is also done because
				// the "defer cancel()" above scheduled
				// before this goroutine.
			default:
				// The TLS or HTTP or DERP exchanges didn't complete
				// in time. Force close the TCP connection to force
				// them to fail quickly.
				tcpConn.Close()
			}
		}
	}()

	var httpConn net.Conn    // a TCP conn or a TLS conn; what we speak HTTP to
	var serverPub key.Public // or zero if unknown (if not using TLS or TLS middlebox eats it)
	var serverProtoVersion int
	if c.useHTTPS() {
		tlsConn := c.tlsClient(tcpConn, node)
		httpConn = tlsConn

		// Force a handshake now (instead of waiting for it to
		// be done implicitly on read/write) so we can check
		// the ConnectionState.
		if err := tlsConn.Handshake(); err != nil {
			return nil, 0, err
		}

		// We expect to be using TLS 1.3 to our own servers, and only
		// starting at TLS 1.3 are the server's returned certificates
		// encrypted, so only look for and use our "meta cert" if we're
		// using TLS 1.3. If we're not using TLS 1.3, it might be a user
		// running cmd/derper themselves with a different configuration,
		// in which case we can avoid this fast-start optimization.
		// (If a corporate proxy is MITM'ing TLS 1.3 connections with
		// corp-mandated TLS root certs than all bets are off anyway.)
		// Note that we're not specifically concerned about TLS downgrade
		// attacks. TLS handles that fine:
		// https://blog.gypsyengineer.com/en/security/how-does-tls-1-3-protect-against-downgrade-attacks.html
		connState := tlsConn.ConnectionState()
		if connState.Version >= tls.VersionTLS13 {
			serverPub, serverProtoVersion = parseMetaCert(connState.PeerCertificates)
		}
	} else {
		httpConn = tcpConn
	}

	brw := bufio.NewReadWriter(bufio.NewReader(httpConn), bufio.NewWriter(httpConn))
	var derpClient *derp.Client

	req, err := http.NewRequest("GET", c.urlString(node), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Upgrade", "DERP")
	req.Header.Set("Connection", "Upgrade")

	if !serverPub.IsZero() && serverProtoVersion != 0 {
		// parseMetaCert found the server's public key (no TLS
		// middlebox was in the way), so skip the HTTP upgrade
		// exchange.  See https://github.com/tailscale/tailscale/issues/693
		// for an overview. We still send the HTTP request
		// just to get routed into the server's HTTP Handler so it
		// can Hijack the request, but we signal with a special header
		// that we don't want to deal with its HTTP response.
		req.Header.Set(fastStartHeader, "1") // suppresses the server's HTTP response
		if err := req.Write(brw); err != nil {
			return nil, 0, err
		}
		// No need to flush the HTTP request. the derp.Client's initial
		// client auth frame will flush it.
	} else {
		if err := req.Write(brw); err != nil {
			return nil, 0, err
		}
		if err := brw.Flush(); err != nil {
			return nil, 0, err
		}

		resp, err := http.ReadResponse(brw.Reader, req)
		if err != nil {
			return nil, 0, err
		}
		if resp.StatusCode != http.StatusSwitchingProtocols {
			b, _ := ioutil.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, 0, fmt.Errorf("GET failed: %v: %s", err, b)
		}
	}
	derpClient, err = derp.NewClient(c.privateKey, httpConn, brw, c.logf,
		derp.MeshKey(c.MeshKey),
		derp.ServerPublicKey(serverPub),
		derp.CanAckPings(c.canAckPings),
		derp.IsProber(c.IsProber),
	)
	if err != nil {
		return nil, 0, err
	}
	if c.preferred {
		if err := derpClient.NotePreferred(true); err != nil {
			go httpConn.Close()
			return nil, 0, err
		}
	}

	c.serverPubKey = derpClient.ServerPublicKey()
	c.client = derpClient
	c.netConn = tcpConn
	c.connGen++
	return c.client, c.connGen, nil
}

func (c *Client) dialURL(ctx context.Context) (net.Conn, error) {
	host := c.url.Hostname()
	hostOrIP := host

	dialer := netns.NewDialer()

	if c.DNSCache != nil {
		ip, _, err := c.DNSCache.LookupIP(ctx, host)
		if err == nil {
			hostOrIP = ip.String()
		}
		if err != nil && netns.IsSOCKSDialer(dialer) {
			// Return an error if we're not using a dial
			// proxy that can do DNS lookups for us.
			return nil, err
		}
	}

	tcpConn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(hostOrIP, urlPort(c.url)))
	if err != nil {
		return nil, fmt.Errorf("dial of %v: %v", host, err)
	}
	return tcpConn, nil
}

// dialRegion returns a TCP connection to the provided region, trying
// each node in order (with dialNode) until one connects or ctx is
// done.
func (c *Client) dialRegion(ctx context.Context, reg *tailcfg.DERPRegion) (net.Conn, *tailcfg.DERPNode, error) {
	if len(reg.Nodes) == 0 {
		return nil, nil, fmt.Errorf("no nodes for %s", c.targetString(reg))
	}
	var firstErr error
	for _, n := range reg.Nodes {
		if n.STUNOnly {
			if firstErr == nil {
				firstErr = fmt.Errorf("no non-STUNOnly nodes for %s", c.targetString(reg))
			}
			continue
		}
		c, err := c.dialNode(ctx, n)
		if err == nil {
			return c, n, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, nil, firstErr
}

func (c *Client) tlsClient(nc net.Conn, node *tailcfg.DERPNode) *tls.Conn {
	tlsConf := tlsdial.Config(c.tlsServerName(node), c.TLSConfig)
	if node != nil {
		tlsConf.InsecureSkipVerify = node.InsecureForTests
		if node.CertName != "" {
			tlsdial.SetConfigExpectedCert(tlsConf, node.CertName)
		}
	}
	if n := os.Getenv("SSLKEYLOGFILE"); n != "" {
		f, err := os.OpenFile(n, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("WARNING: writing to SSLKEYLOGFILE %v", n)
		tlsConf.KeyLogWriter = f
	}
	return tls.Client(nc, tlsConf)
}

func (c *Client) DialRegionTLS(ctx context.Context, reg *tailcfg.DERPRegion) (tlsConn *tls.Conn, connClose io.Closer, err error) {
	tcpConn, node, err := c.dialRegion(ctx, reg)
	if err != nil {
		return nil, nil, err
	}
	done := make(chan bool) // unbufferd
	defer close(done)

	tlsConn = c.tlsClient(tcpConn, node)
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			tcpConn.Close()
		}
	}()
	err = tlsConn.Handshake()
	if err != nil {
		return nil, nil, err
	}
	select {
	case done <- true:
		return tlsConn, tcpConn, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (c *Client) dialContext(ctx context.Context, proto, addr string) (net.Conn, error) {
	return netns.NewDialer().DialContext(ctx, proto, addr)
}

// shouldDialProto reports whether an explicitly provided IPv4 or IPv6
// address (given in s) is valid. An empty value means to dial, but to
// use DNS. The predicate function reports whether the non-empty
// string s contained a valid IP address of the right family.
func shouldDialProto(s string, pred func(netaddr.IP) bool) bool {
	if s == "" {
		return true
	}
	ip, _ := netaddr.ParseIP(s)
	return pred(ip)
}

const dialNodeTimeout = 1500 * time.Millisecond

// dialNode returns a TCP connection to node n, racing IPv4 and IPv6
// (both as applicable) against each other.
// A node is only given dialNodeTimeout to connect.
//
// TODO(bradfitz): longer if no options remain perhaps? ...  Or longer
// overall but have dialRegion start overlapping races?
func (c *Client) dialNode(ctx context.Context, n *tailcfg.DERPNode) (net.Conn, error) {
	// First see if we need to use an HTTP proxy.
	proxyReq := &http.Request{
		Method: "GET", // doesn't really matter
		URL: &url.URL{
			Scheme: "https",
			Host:   c.tlsServerName(n),
			Path:   "/", // unused
		},
	}
	if proxyURL, err := tshttpproxy.ProxyFromEnvironment(proxyReq); err == nil && proxyURL != nil {
		return c.dialNodeUsingProxy(ctx, n, proxyURL)
	}

	type res struct {
		c   net.Conn
		err error
	}
	resc := make(chan res) // must be unbuffered
	ctx, cancel := context.WithTimeout(ctx, dialNodeTimeout)
	defer cancel()

	nwait := 0
	startDial := func(dstPrimary, proto string) {
		nwait++
		go func() {
			dst := dstPrimary
			if dst == "" {
				dst = n.HostName
			}
			port := "443"
			if n.DERPPort != 0 {
				port = fmt.Sprint(n.DERPPort)
			}
			c, err := c.dialContext(ctx, proto, net.JoinHostPort(dst, port))
			select {
			case resc <- res{c, err}:
			case <-ctx.Done():
				if c != nil {
					c.Close()
				}
			}
		}()
	}
	if shouldDialProto(n.IPv4, netaddr.IP.Is4) {
		startDial(n.IPv4, "tcp4")
	}
	if shouldDialProto(n.IPv6, netaddr.IP.Is6) {
		startDial(n.IPv6, "tcp6")
	}
	if nwait == 0 {
		return nil, errors.New("both IPv4 and IPv6 are explicitly disabled for node")
	}

	var firstErr error
	for {
		select {
		case res := <-resc:
			nwait--
			if res.err == nil {
				return res.c, nil
			}
			if firstErr == nil {
				firstErr = res.err
			}
			if nwait == 0 {
				return nil, firstErr
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func firstStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// dialNodeUsingProxy connects to n using a CONNECT to the HTTP(s) proxy in proxyURL.
func (c *Client) dialNodeUsingProxy(ctx context.Context, n *tailcfg.DERPNode, proxyURL *url.URL) (proxyConn net.Conn, err error) {
	pu := proxyURL
	if pu.Scheme == "https" {
		var d tls.Dialer
		proxyConn, err = d.DialContext(ctx, "tcp", net.JoinHostPort(pu.Hostname(), firstStr(pu.Port(), "443")))
	} else {
		var d net.Dialer
		proxyConn, err = d.DialContext(ctx, "tcp", net.JoinHostPort(pu.Hostname(), firstStr(pu.Port(), "80")))
	}
	defer func() {
		if err != nil && proxyConn != nil {
			// In a goroutine in case it's a *tls.Conn (that can block on Close)
			// TODO(bradfitz): track the underlying tcp.Conn and just close that instead.
			go proxyConn.Close()
		}
	}()
	if err != nil {
		return nil, err
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
			proxyConn.Close()
		}
	}()

	target := net.JoinHostPort(n.HostName, "443")

	var authHeader string
	if v, err := tshttpproxy.GetAuthHeader(pu); err != nil {
		c.logf("derphttp: error getting proxy auth header for %v: %v", proxyURL, err)
	} else if v != "" {
		authHeader = fmt.Sprintf("Proxy-Authorization: %s\r\n", v)
	}

	if _, err := fmt.Fprintf(proxyConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, pu.Hostname(), authHeader); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}

	br := bufio.NewReader(proxyConn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.logf("derphttp: CONNECT dial to %s: %v", target, err)
		return nil, err
	}
	c.logf("derphttp: CONNECT dial to %s: %v", target, res.Status)
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("invalid response status from HTTP proxy %s on CONNECT to %s: %v", pu, target, res.Status)
	}
	return proxyConn, nil
}

func (c *Client) Send(dstKey key.Public, b []byte) error {
	client, _, err := c.connect(context.TODO(), "derphttp.Client.Send")
	if err != nil {
		return err
	}
	if err := client.Send(dstKey, b); err != nil {
		c.closeForReconnect(client)
	}
	return err
}

func (c *Client) ForwardPacket(from, to key.Public, b []byte) error {
	client, _, err := c.connect(context.TODO(), "derphttp.Client.ForwardPacket")
	if err != nil {
		return err
	}
	if err := client.ForwardPacket(from, to, b); err != nil {
		c.closeForReconnect(client)
	}
	return err
}

// SendPong sends a reply to a ping, with the ping's provided
// challenge/identifier data.
//
// Unlike other send methods, SendPong makes no attempt to connect or
// reconnect to the peer. It's best effort. If there's a connection
// problem, the server will choose to hang up on us if we're not
// replying.
func (c *Client) SendPong(data [8]byte) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClientClosed
	}
	if c.client == nil {
		c.mu.Unlock()
		return errors.New("not connected")
	}
	dc := c.client
	c.mu.Unlock()

	return dc.SendPong(data)
}

// SetCanAckPings sets whether this client will reply to ping requests from the server.
//
// This only affects future connections.
func (c *Client) SetCanAckPings(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.canAckPings = v
}

// NotePreferred notes whether this Client is the caller's preferred
// (home) DERP node. It's only used for stats.
func (c *Client) NotePreferred(v bool) {
	c.mu.Lock()
	if c.preferred == v {
		c.mu.Unlock()
		return
	}
	c.preferred = v
	client := c.client
	c.mu.Unlock()

	if client != nil {
		if err := client.NotePreferred(v); err != nil {
			c.closeForReconnect(client)
		}
	}
}

// WatchConnectionChanges sends a request to subscribe to
// notifications about clients connecting & disconnecting.
//
// Only trusted connections (using MeshKey) are allowed to use this.
func (c *Client) WatchConnectionChanges() error {
	client, _, err := c.connect(context.TODO(), "derphttp.Client.WatchConnectionChanges")
	if err != nil {
		return err
	}
	err = client.WatchConnectionChanges()
	if err != nil {
		c.closeForReconnect(client)
	}
	return err
}

// ClosePeer asks the server to close target's TCP connection.
//
// Only trusted connections (using MeshKey) are allowed to use this.
func (c *Client) ClosePeer(target key.Public) error {
	client, _, err := c.connect(context.TODO(), "derphttp.Client.ClosePeer")
	if err != nil {
		return err
	}
	err = client.ClosePeer(target)
	if err != nil {
		c.closeForReconnect(client)
	}
	return err
}

// Recv reads a message from c. The returned message may alias memory from Client.
// The message should only be used until the next Client call.
func (c *Client) Recv() (derp.ReceivedMessage, error) {
	m, _, err := c.RecvDetail()
	return m, err
}

// RecvDetail is like Recv, but additional returns the connection generation on each message.
// The connGen value is incremented every time the derphttp.Client reconnects to the server.
func (c *Client) RecvDetail() (m derp.ReceivedMessage, connGen int, err error) {
	client, connGen, err := c.connect(context.TODO(), "derphttp.Client.Recv")
	if err != nil {
		return nil, 0, err
	}
	m, err = client.Recv()
	if err != nil {
		c.closeForReconnect(client)
		if c.isClosed() {
			err = ErrClientClosed
		}
	}
	return m, connGen, err
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Close closes the client. It will not automatically reconnect after
// being closed.
func (c *Client) Close() error {
	c.cancelCtx() // not in lock, so it can cancel Connect, which holds mu

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClientClosed
	}
	c.closed = true
	if c.netConn != nil {
		c.netConn.Close()
	}
	return nil
}

// closeForReconnect closes the underlying network connection and
// zeros out the client field so future calls to Connect will
// reconnect.
//
// The provided brokenClient is the client to forget. If current
// client is not brokenClient, closeForReconnect does nothing. (This
// prevents a send and receive goroutine from failing at the ~same
// time and both calling closeForReconnect and the caller goroutines
// forever calling closeForReconnect in lockstep endlessly;
// https://github.com/tailscale/tailscale/pull/264)
func (c *Client) closeForReconnect(brokenClient *derp.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != brokenClient {
		return
	}
	if c.netConn != nil {
		c.netConn.Close()
		c.netConn = nil
	}
	c.client = nil
}

var ErrClientClosed = errors.New("derphttp.Client closed")

func parseMetaCert(certs []*x509.Certificate) (serverPub key.Public, serverProtoVersion int) {
	for _, cert := range certs {
		if cn := cert.Subject.CommonName; strings.HasPrefix(cn, "derpkey") {
			var err error
			serverPub, err = key.NewPublicFromHexMem(mem.S(strings.TrimPrefix(cn, "derpkey")))
			if err == nil && cert.SerialNumber.BitLen() <= 8 { // supports up to version 255
				return serverPub, int(cert.SerialNumber.Int64())
			}
		}
	}
	return key.Public{}, 0
}
