// Package fetch retrieves public URLs for members: a file named by url in a
// create or update, and the page and assets of an import. It dials public
// addresses only, so a member cannot make the server probe its own network,
// and it caps how much it reads and how long it waits.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
)

const (
	DefaultTimeout = 15 * time.Second
	DefaultBudget  = time.Minute
	maxRedirects   = 5
	spillPrefix    = "kazari-fetch-"
)

// Options configures a Client.
type Options struct {
	// Timeout bounds one request; Budget bounds every request of one import.
	Timeout, Budget time.Duration
	// Dir holds downloaded files until the publish that uses them ends.
	Dir       string
	UserAgent string
	// Transport replaces the public-only transport, for tests.
	Transport http.RoundTripper
}

// Client fetches URLs under Options.
type Client struct {
	hc   *http.Client
	opts Options
}

// New returns a Client; zero options take the defaults.
func New(opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget
	}
	if opts.Dir == "" {
		opts.Dir = os.TempDir()
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "Kazari (+https://kazari.io)"
	}
	tr := opts.Transport
	if tr == nil {
		tr = Transport()
	}
	return &Client{opts: opts, hc: &http.Client{
		Timeout:   opts.Timeout,
		Transport: tr,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("more than %d redirects", maxRedirects)
			}
			return nil
		},
	}}
}

// Budget is how long one import may take in all.
func (c *Client) Budget() time.Duration { return c.opts.Budget }

// Result describes a fetched resource.
type Result struct {
	URL         *url.URL // after redirects
	ContentType string
}

// Get returns the body of u, which must be at most limit bytes.
func (c *Client) Get(ctx context.Context, u string, limit int64) ([]byte, Result, error) {
	resp, res, err := c.get(ctx, u)
	if err != nil {
		return nil, Result{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, Result{}, failed(u, err)
	}
	if int64(len(data)) > limit {
		return nil, Result{}, tooLarge(u, limit)
	}
	return data, res, nil
}

// File is a downloaded body in a temporary file, positioned at its start.
type File struct {
	*os.File
	Result
	Size int64
}

// Close removes the file.
func (f *File) Close() error {
	err := f.File.Close()
	if rerr := os.Remove(f.Name()); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		err = errors.Join(err, rerr)
	}
	return err
}

// Download writes the body of u, at most limit bytes, to a file under Dir.
func (c *Client) Download(ctx context.Context, u string, limit int64) (*File, error) {
	resp, res, err := c.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(c.opts.Dir, spillPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("fetch: spill: %w", err)
	}
	out := &File{File: f, Result: res}
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	switch {
	case err != nil:
		out.Close()
		return nil, failed(u, err)
	case n > limit:
		out.Close()
		return nil, tooLarge(u, limit)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		out.Close()
		return nil, fmt.Errorf("fetch: spill: %w", err)
	}
	out.Size = n
	return out, nil
}

func (c *Client) get(ctx context.Context, u string) (*http.Response, Result, error) {
	parsed, err := url.Parse(strings.TrimSpace(u))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, Result{}, artifact.Errorf(artifact.ErrInvalidArgument, "url %q must be an http or https URL", u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, Result{}, artifact.Errorf(artifact.ErrInvalidArgument, "url %q: %v", u, err)
	}
	req.Header.Set("User-Agent", c.opts.UserAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, Result{}, failed(u, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, Result{}, artifact.Errorf(artifact.ErrFetchFailed, "%s answered %s", u, resp.Status)
	}
	return resp, Result{URL: resp.Request.URL, ContentType: resp.Header.Get("Content-Type")}, nil
}

func failed(u string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return artifact.Errorf(artifact.ErrFetchFailed, "%s: %v", u, err)
}

func tooLarge(u string, limit int64) error {
	return artifact.Errorf(artifact.ErrFetchFailed, "%s is larger than %d bytes", u, limit)
}

// Transport dials public addresses only. The check runs on the resolved
// address and on every redirect, so a name pointing at a private address
// is refused as well.
func Transport() *http.Transport {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if !PublicAddr(host) {
				return fmt.Errorf("non-public address %s", host)
			}
			return nil
		},
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = dialer.DialContext
	tr.Proxy = nil
	return tr
}

// nonPublic are special-purpose ranges the netip predicates miss, among them
// carrier-grade NAT (Tailscale) and IPv6 transition prefixes that can tunnel
// to an internal IPv4 address.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

// PublicAddr reports whether host, a literal IP, is public unicast.
func PublicAddr(host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// RemoveStale removes downloads in dir older than before, which a crash left behind.
func RemoveStale(dir string, before time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("fetch: remove stale downloads: %w", err)
	}
	var (
		n    int
		errs []error
	)
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), spillPrefix) {
			continue
		}
		info, err := e.Info()
		if err == nil && !info.ModTime().Before(before) {
			continue
		}
		if err == nil {
			err = os.Remove(filepath.Join(dir, e.Name()))
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("fetch: remove stale download: %w", err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
