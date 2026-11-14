package netguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var (
	ErrBlockedHost    = errors.New("ssrf guard: host not in allowlist")
	ErrBlockedAddress = errors.New("ssrf guard: non-public IP address blocked")
	ErrTooManyHops    = errors.New("ssrf guard: redirect limit exceeded")
)

var defaultAllowedHosts = map[string]bool{
	"www.youtube.com":       true,
	"youtube.com":           true,
	"youtu.be":              true,
	"www.googleapis.com":    true,
	"www.tiktok.com":        true,
	"tiktok.com":            true,
	"vm.tiktok.com":         true,
	"vt.tiktok.com":         true,
	"open.tiktokapis.com":   true,
	"www.facebook.com":      true,
	"facebook.com":          true,
	"fb.watch":              true,
	"graph.facebook.com":    true,
	"www.instagram.com":     true,
	"instagram.com":         true,
	"graph.instagram.com":   true,
}

// Options configures the SSRF-guarded HTTP client.
type Options struct {
	AllowedHosts      map[string]bool
	Timeout           time.Duration
	MaxResponseBytes  int64
	MaxRedirects      int
	AllowLoopbackTest bool // Strictly for unit tests against local httptest.Server
}

type SafeClient struct {
	httpClient       *http.Client
	allowedHosts     map[string]bool
	maxResponseBytes int64
}

func NewSafeClient(opts Options) *SafeClient {
	allowed := make(map[string]bool, len(defaultAllowedHosts))
	for k, v := range defaultAllowedHosts {
		allowed[k] = v
	}
	for k, v := range opts.AllowedHosts {
		allowed[strings.ToLower(k)] = v
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	maxBytes := opts.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = 2 * 1024 * 1024 // 2 MiB ceiling
	}
	maxRedirects := opts.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = 4
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
	transport := &http.Transport{
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("%w: no IPs resolved for %s", ErrBlockedAddress, host)
			}
			for _, ip := range ips {
				if !IsPublicAddr(ip) && !(opts.AllowLoopbackTest && ip.IsLoopback()) {
					return nil, fmt.Errorf("%w: %s resolved to restricted address %s", ErrBlockedAddress, host, ip.String())
				}
			}
			// Connect specifically to the validated IP so DNS rebinding cannot swap targets mid-flight.
			target := net.JoinHostPort(ips[0].String(), port)
			return dialer.DialContext(ctx, network, target)
		},
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return ErrTooManyHops
			}
			return validateRequestTarget(req.URL, allowed)
		},
	}

	return &SafeClient{
		httpClient:       client,
		allowedHosts:     allowed,
		maxResponseBytes: maxBytes,
	}
}

func (c *SafeClient) GetBytes(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, http.Header, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, 0, nil, err
	}
	if err := validateRequestTarget(u, c.allowedHosts); err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("User-Agent", "VueltarioMetadataBot/1.0 (+https://vueltario.com)")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, c.maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, resp.StatusCode, resp.Header, err
	}
	if int64(len(body)) > c.maxResponseBytes {
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("ssrf guard: response exceeded %d byte limit", c.maxResponseBytes)
	}
	return body, resp.StatusCode, resp.Header, nil
}

func validateRequestTarget(u *url.URL, allowed map[string]bool) error {
	if u == nil {
		return ErrBlockedHost
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("ssrf guard: unsupported scheme %q", u.Scheme)
	}
	if u.User != nil {
		return errors.New("ssrf guard: userinfo in URL is forbidden")
	}
	host := strings.ToLower(u.Hostname())
	if !allowed[host] {
		return fmt.Errorf("%w: %s", ErrBlockedHost, host)
	}
	return nil
}

var (
	cgnatPrefix    = netip.MustParsePrefix("100.64.0.0/10")
	benchmarkNet   = netip.MustParsePrefix("198.18.0.0/15")
	docNet1        = netip.MustParsePrefix("192.0.2.0/24")
	docNet2        = netip.MustParsePrefix("198.51.100.0/24")
	docNet3        = netip.MustParsePrefix("203.0.113.0/24")
	ipv6DocPrefix  = netip.MustParsePrefix("2001:db8::/32")
	metadataPrefix = netip.MustParsePrefix("169.254.0.0/16")
)

// IsPublicAddr returns true only for globally routable unicast addresses.
// Rejects loopback, RFC1918 private, cloud metadata/link-local (169.254.169.254), CGNAT, multicast, and unspecified IPs.
func IsPublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if cgnatPrefix.Contains(ip) || benchmarkNet.Contains(ip) || metadataPrefix.Contains(ip) ||
		docNet1.Contains(ip) || docNet2.Contains(ip) || docNet3.Contains(ip) || ipv6DocPrefix.Contains(ip) {
		return false
	}
	return true
}
