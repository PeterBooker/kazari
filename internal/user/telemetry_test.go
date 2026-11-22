package user_test

import (
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/PeterBooker/kazari/internal/instrument/instrumenttest"
	"github.com/PeterBooker/kazari/internal/user"
)

func TestAccountTelemetry(t *testing.T) {
	rec := instrumenttest.New(t)
	f := accounts(t, user.Options{Telemetry: rec.Providers})
	ctx, end := rec.Start("request")

	_, _, token, err := f.svc.Invite(ctx, user.InviteRequest{Handle: "ana", Email: "ana@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CheckPassword(ctx, "ana", goodPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CheckPassword(ctx, "nobody", goodPassword); err == nil {
		t.Fatal("an unknown name signed in")
	}
	reset, _, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RevokeLink(ctx, reset.ID); err != nil {
		t.Fatal(err)
	}
	end()

	const links = "kazari.user.link.operations"
	purpose, operation := attribute.Key("kazari.user.link.purpose"), attribute.Key("kazari.user.link.operation")
	rec.Want(links, 1, purpose.String("invite"), operation.String("create"))
	rec.Want(links, 1, purpose.String("invite"), operation.String("use"))
	rec.Want(links, 1, purpose.String("reset"), operation.String("create"))
	rec.Want(links, 1, purpose.String("reset"), operation.String("revoke"))
	rec.Want(links, 4)

	// An unknown name costs a verification like a real one, and is timed
	// like one.
	const hashes = "kazari.user.password.hash.duration"
	rec.Want(hashes, 1, attribute.String("kazari.user.password.operation", "hash"))
	rec.Want(hashes, 2, attribute.String("kazari.user.password.operation", "verify"))

	if got := rec.SpanAttr("user.redeem_link", purpose); got != "invite" {
		t.Errorf("user.redeem_link purpose = %q, want invite", got)
	}
	// Who did what stays in the audit log: no span names a person, a link
	// or a password.
	for _, name := range []string{"user.invite", "user.redeem_link", "user.check_password", "user.issue_link", "user.revoke_link"} {
		span, ok := rec.Span(name)
		if !ok {
			t.Errorf("no span %s", name)
			continue
		}
		for _, kv := range span.Attributes() {
			v := kv.Value.String()
			if strings.Contains(v, "ana") || strings.Contains(v, token) || strings.Contains(v, goodPassword) {
				t.Errorf("span %s attribute %s = %q", name, kv.Key, v)
			}
		}
	}
}
