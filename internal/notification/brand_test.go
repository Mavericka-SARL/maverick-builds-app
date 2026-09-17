package notification

import (
	"context"
	"testing"
)

type brandedFake struct {
	fromName, subject string
	branded           bool
}

func (f *brandedFake) Send(_ context.Context, _, _, subject, _ string) error {
	f.subject, f.branded = subject, false
	return nil
}
func (f *brandedFake) SendAs(_ context.Context, fromName, _, _, subject, _ string) error {
	f.fromName, f.subject, f.branded = fromName, subject, true
	return nil
}

// A white-labelled tenant's mail carries its name: as the sender's display
// name and in the fallback subject. Without a brand nothing changes.
func TestDeliverBrandsEmail(t *testing.T) {
	fake := &brandedFake{}
	d := &Dispatcher{Mailer: fake, BrandName: func(context.Context, string) string { return "Acme Planning" }}
	m := Message{Channel: "email", Email: "a@acme.test", Vars: map[string]string{"message": "hello"}}
	if err := d.deliver(context.Background(), Settings{EmailEnabled: true}, m); err != nil {
		t.Fatal(err)
	}
	if !fake.branded || fake.fromName != "Acme Planning" || fake.subject != "Notification from Acme Planning" {
		t.Fatalf("branded delivery: %+v", fake)
	}
	m.Vars["subject"] = "Approve the budget"
	_ = d.deliver(context.Background(), Settings{EmailEnabled: true}, m)
	if fake.subject != "Approve the budget" {
		t.Errorf("an explicit subject must win: %q", fake.subject)
	}
	plain := &brandedFake{}
	d = &Dispatcher{Mailer: plain}
	delete(m.Vars, "subject")
	_ = d.deliver(context.Background(), Settings{EmailEnabled: true}, m)
	if plain.branded || plain.subject != "Notification from maverickbuilds.app" {
		t.Errorf("unbranded delivery: %+v", plain)
	}
}
