package certs

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/libdns/libdns"
	"github.com/miekg/dns"
)

// recordingDNS remembers what was written and deleted.
type recordingDNS struct {
	appendErr, deleteErr error
	zone                 string
	added, deleted       []libdns.Record
}

func (r *recordingDNS) AppendRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	if r.appendErr != nil {
		return nil, r.appendErr
	}
	r.zone = zone
	r.added = append(r.added, recs...)
	return recs, nil
}

func (r *recordingDNS) DeleteRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	if r.deleteErr != nil {
		return nil, r.deleteErr
	}
	r.deleted = append(r.deleted, recs...)
	return recs, nil
}

func TestCheckDNSProvider(t *testing.T) {
	p := &recordingDNS{}
	if err := CheckDNSProvider(context.Background(), p, "example.com.", "art.example.com"); err != nil {
		t.Fatal(err)
	}
	if p.zone != "example.com." || len(p.added) != 1 || len(p.deleted) != 1 {
		t.Fatalf("zone %q, added %v, deleted %v", p.zone, p.added, p.deleted)
	}
	txt, ok := p.added[0].(libdns.TXT)
	if !ok || txt.Name != "_kazari-check.art" || len(txt.Text) != 32 {
		t.Errorf("added %#v, want a _kazari-check.art TXT with a 32-char value", p.added[0])
	}
	if p.deleted[0] != p.added[0] {
		t.Errorf("deleted %v, want the added record", p.deleted[0])
	}

	boom := errors.New("403 forbidden")
	err := CheckDNSProvider(context.Background(), &recordingDNS{appendErr: boom}, "example.com.", "art.example.com")
	if err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), "write _kazari-check.art.example.com TXT in zone example.com.") {
		t.Errorf("append error = %v", err)
	}
	err = CheckDNSProvider(context.Background(), &recordingDNS{deleteErr: boom}, "example.com.", "art.example.com")
	if err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), "delete _kazari-check.art.example.com TXT") {
		t.Errorf("delete error = %v", err)
	}
}

// soaServer answers SOA for zone and nothing for other names, like a
// resolver for a domain whose records live in zone.
func soaServer(t *testing.T, zone string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(req)
		if q := req.Question[0]; q.Qtype == dns.TypeSOA && q.Name == zone {
			m.Answer = append(m.Answer, &dns.SOA{
				Hdr: dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
				Ns:  "ns1." + zone, Mbox: "hostmaster." + zone, Serial: 1, Refresh: 3600, Retry: 600, Expire: 86400, Minttl: 60,
			})
		}
		w.WriteMsg(m)
	})}
	go srv.ActivateAndServe()
	t.Cleanup(func() { srv.Shutdown() })
	return pc.LocalAddr().String()
}

func TestFindZone(t *testing.T) {
	ns := soaServer(t, "example.com.")
	zone, err := FindZone(context.Background(), "art.example.com", []string{ns})
	if err != nil || zone != "example.com." {
		t.Fatalf("FindZone = %q, %v; want example.com.", zone, err)
	}
	if _, err := FindZone(context.Background(), "art.example.net", []string{ns}); err == nil {
		t.Error("FindZone found a zone for a name no server holds")
	}
}
