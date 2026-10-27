package certs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/libdns"
)

// checkLabel is the TXT record CheckDNSProvider writes under the base
// domain, beside the _acme-challenge record issuance writes.
const checkLabel = "_kazari-check"

// FindZone returns the DNS zone that holds name, with a trailing dot, as the
// DNS-01 solver finds it: the nearest parent whose nameserver answers a SOA
// query. nameservers are "host:port"; empty uses the system resolvers.
func FindZone(ctx context.Context, name string, nameservers []string) (string, error) {
	zone, err := certmagic.FindZoneByFQDN(ctx, newZapLogger(slog.New(slog.DiscardHandler)), name,
		certmagic.RecursiveNameservers(nameservers))
	if err != nil {
		return "", fmt.Errorf("certs: find zone of %s: %w", name, err)
	}
	return zone, nil
}

// CheckDNSProvider proves p can do what issuance needs: it writes a TXT
// record _kazari-check.<name> with a random value in zone and deletes it
// again. The record it wrote is removed even when the check fails.
func CheckDNSProvider(ctx context.Context, p certmagic.DNSProvider, zone, name string) error {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Errorf("certs: dns check: %w", err)
	}
	rec := libdns.TXT{
		Name: libdns.RelativeName(checkLabel+"."+name+".", zone),
		Text: hex.EncodeToString(buf[:]),
		TTL:  time.Minute,
	}
	added, err := p.AppendRecords(ctx, zone, []libdns.Record{rec})
	if err != nil {
		return fmt.Errorf("certs: dns check: write %s TXT in zone %s: %w", checkLabel+"."+name, zone, err)
	}
	if len(added) == 0 {
		added = []libdns.Record{rec}
	}
	if _, err := p.DeleteRecords(ctx, zone, added); err != nil {
		return fmt.Errorf("certs: dns check: delete %s TXT in zone %s: %w", checkLabel+"."+name, zone, err)
	}
	return nil
}
