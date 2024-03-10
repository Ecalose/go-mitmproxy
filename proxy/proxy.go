package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/url"
)

type Options struct {
	Debug             int
	Addr              string
	StreamLargeBodies int64 // 当请求或响应体大于此字节时，转为 stream 模式
	SslInsecure       bool
	CaRootPath        string
	Upstream          string
}

type Proxy struct {
	Opts    *Options
	Version string
	Addons  []Addon

	entry           *entry
	client          *http.Client
	interceptor     *middle
	shouldIntercept func(req *http.Request) bool              // req is received by proxy.server
	upstreamProxy   func(req *http.Request) (*url.URL, error) // req is received by proxy.server, not client request
}

// proxy.server req context key
var proxyReqCtxKey = new(struct{})

func NewProxy(opts *Options) (*Proxy, error) {
	if opts.StreamLargeBodies <= 0 {
		opts.StreamLargeBodies = 1024 * 1024 * 5 // default: 5mb
	}

	proxy := &Proxy{
		Opts:    opts,
		Version: "1.7.1",
		Addons:  make([]Addon, 0),
	}

	proxy.entry = newEntry(proxy, opts.Addr)

	proxy.client = &http.Client{
		Transport: &http.Transport{
			Proxy:              proxy.realUpstreamProxy(),
			ForceAttemptHTTP2:  false, // disable http2
			DisableCompression: true,  // To get the original response from the server, set Transport.DisableCompression to true.
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: opts.SslInsecure,
				KeyLogWriter:       getTlsKeyLogWriter(),
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 禁止自动重定向
			return http.ErrUseLastResponse
		},
	}

	interceptor, err := newMiddle(proxy)
	if err != nil {
		return nil, err
	}
	proxy.interceptor = interceptor

	return proxy, nil
}

func (proxy *Proxy) AddAddon(addon Addon) {
	proxy.Addons = append(proxy.Addons, addon)
}

func (proxy *Proxy) Start() error {
	go proxy.interceptor.start()
	return proxy.entry.start()
}

func (proxy *Proxy) Close() error {
	proxy.interceptor.close()
	return proxy.entry.close()
}

func (proxy *Proxy) Shutdown(ctx context.Context) error {
	proxy.interceptor.close()
	return proxy.entry.shutdown(ctx)
}

func (proxy *Proxy) GetCertificate() x509.Certificate {
	return proxy.interceptor.ca.RootCert
}

func (proxy *Proxy) SetShouldInterceptRule(rule func(req *http.Request) bool) {
	proxy.shouldIntercept = rule
}

func (proxy *Proxy) SetUpstreamProxy(fn func(req *http.Request) (*url.URL, error)) {
	proxy.upstreamProxy = fn
}

func (proxy *Proxy) realUpstreamProxy() func(*http.Request) (*url.URL, error) {
	return func(cReq *http.Request) (*url.URL, error) {
		req := cReq.Context().Value(proxyReqCtxKey).(*http.Request)
		return proxy.getUpstreamProxyUrl(req)
	}
}

func (proxy *Proxy) getUpstreamProxyUrl(req *http.Request) (*url.URL, error) {
	if proxy.upstreamProxy != nil {
		return proxy.upstreamProxy(req)
	}
	if len(proxy.Opts.Upstream) > 0 {
		return url.Parse(proxy.Opts.Upstream)
	}
	cReq := &http.Request{URL: &url.URL{Scheme: "https", Host: req.Host}}
	return http.ProxyFromEnvironment(cReq)
}

func (proxy *Proxy) getUpstreamConn(req *http.Request) (net.Conn, error) {
	proxyUrl, err := proxy.getUpstreamProxyUrl(req)
	if err != nil {
		return nil, err
	}
	var conn net.Conn
	if proxyUrl != nil {
		conn, err = getProxyConn(proxyUrl, req.Host)
	} else {
		conn, err = (&net.Dialer{}).DialContext(context.Background(), "tcp", req.Host)
	}
	return conn, err
}
