package telemetry

import (
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	collogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// received is one export as an OTLP receiver saw it.
type received struct {
	path     string
	header   http.Header
	encoding string
	body     []byte
	clientCN string
}

// httpReceiver is an OTLP/HTTP collector that answers status, or 200 with
// an empty protobuf response.
type httpReceiver struct {
	mu     sync.Mutex
	got    []received
	status int
}

func (h *httpReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body = zr
	}
	data, _ := io.ReadAll(body)
	rec := received{path: r.URL.Path, header: r.Header.Clone(), encoding: r.Header.Get("Content-Encoding"), body: data}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		rec.clientCN = r.TLS.PeerCertificates[0].Subject.CommonName
	}
	h.mu.Lock()
	h.got = append(h.got, rec)
	status := h.status
	h.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func (h *httpReceiver) requests() []received {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]received(nil), h.got...)
}

// grpcReceiver is an OTLP/gRPC collector.
type grpcReceiver struct {
	colmetricpb.UnimplementedMetricsServiceServer
	mu      sync.Mutex
	md      []metadata.MD
	metrics []*colmetricpb.ExportMetricsServiceRequest
	traces  []*coltracepb.ExportTraceServiceRequest
	logs    []*collogpb.ExportLogsServiceRequest
}

type grpcLogs struct {
	collogpb.UnimplementedLogsServiceServer
	g *grpcReceiver
}

func (s grpcLogs) Export(ctx context.Context, req *collogpb.ExportLogsServiceRequest) (*collogpb.ExportLogsServiceResponse, error) {
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	s.g.logs = append(s.g.logs, req)
	return &collogpb.ExportLogsServiceResponse{}, nil
}

type grpcTraces struct {
	coltracepb.UnimplementedTraceServiceServer
	g *grpcReceiver
}

func (s grpcTraces) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	s.g.md = append(s.g.md, md)
	s.g.traces = append(s.g.traces, req)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (g *grpcReceiver) Export(ctx context.Context, req *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.md = append(g.md, md)
	g.metrics = append(g.metrics, req)
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

// startGRPC serves g on a loopback port and returns its address.
func startGRPC(t *testing.T, g *grpcReceiver, opts ...grpc.ServerOption) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(opts...)
	colmetricpb.RegisterMetricsServiceServer(srv, g)
	coltracepb.RegisterTraceServiceServer(srv, grpcTraces{g: g})
	collogpb.RegisterLogsServiceServer(srv, grpcLogs{g: g})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

func decodeMetrics(t *testing.T, body []byte) *colmetricpb.ExportMetricsServiceRequest {
	t.Helper()
	var req colmetricpb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode metrics request: %v", err)
	}
	return &req
}

func decodeLogs(t *testing.T, body []byte) *collogpb.ExportLogsServiceRequest {
	t.Helper()
	var req collogpb.ExportLogsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode logs request: %v", err)
	}
	return &req
}

func decodeTraces(t *testing.T, body []byte) *coltracepb.ExportTraceServiceRequest {
	t.Helper()
	var req coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode trace request: %v", err)
	}
	return &req
}

// metricNames lists the metrics in an export with their resource's
// service.name.
func metricNames(req *colmetricpb.ExportMetricsServiceRequest) (service string, names map[string]bool) {
	names = map[string]bool{}
	for _, rm := range req.ResourceMetrics {
		for _, kv := range rm.GetResource().GetAttributes() {
			if kv.Key == "service.name" {
				service = kv.GetValue().GetStringValue()
			}
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				names[m.Name] = true
			}
		}
	}
	return service, names
}

// testPKI is a CA with a server certificate for 127.0.0.1 and client
// certificates, written as PEM files.
type testPKI struct {
	dir    string
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caFile string
	serial int64
}

func newPKI(t *testing.T) *testPKI {
	t.Helper()
	p := &testPKI{dir: t.TempDir()}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p.ca, _ = x509.ParseCertificate(der)
	p.caKey = key
	p.caFile = filepath.Join(p.dir, "ca.pem")
	writePEM(t, p.caFile, "CERTIFICATE", der, time.Now())
	p.serial = 1
	return p
}

func (p *testPKI) issue(t *testing.T, cn string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.serial++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(p.serial),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// writeClient writes a client certificate for cn to cert.pem and key.pem
// with modification time mtime.
func (p *testPKI) writeClient(t *testing.T, cn string, mtime time.Time) (certFile, keyFile string) {
	t.Helper()
	c := p.issue(t, cn, x509.ExtKeyUsageClientAuth)
	keyDER, err := x509.MarshalECPrivateKey(c.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(p.dir, "client.pem"), filepath.Join(p.dir, "client-key.pem")
	writePEM(t, certFile, "CERTIFICATE", c.Certificate[0], mtime)
	writePEM(t, keyFile, "EC PRIVATE KEY", keyDER, mtime)
	return certFile, keyFile
}

func (p *testPKI) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	return pool
}

func writePEM(t *testing.T, path, typ string, der []byte, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// startTLS serves h over TLS with a certificate from p, requiring a client
// certificate from p when mutual is set.
func startTLS(t *testing.T, p *testPKI, h http.Handler, mutual bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{p.issue(t, "collector", x509.ExtKeyUsageServerAuth)}}
	if mutual {
		srv.TLS.ClientAuth = tls.RequireAndVerifyClientCert
		srv.TLS.ClientCAs = p.pool()
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}
