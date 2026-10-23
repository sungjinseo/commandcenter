package meta

import (
	"context"
	"testing"
)

func TestValidateSMTP(t *testing.T) {
	ok := SMTP{Host: "smtp.example.com", Port: 587, Security: "starttls", From: "Command Center <cc@example.com>", Enabled: true}
	if err := ValidateSMTP(ok); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*SMTP){
		"no host":      func(c *SMTP) { c.Host = " " },
		"port zero":    func(c *SMTP) { c.Port = 0 },
		"port big":     func(c *SMTP) { c.Port = 65536 },
		"bad security": func(c *SMTP) { c.Security = "ssl" },
		"empty from":   func(c *SMTP) { c.From = "" },
		"bad from":     func(c *SMTP) { c.From = "not an address" },
	} {
		c := ok
		mut(&c)
		if err := ValidateSMTP(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Disabled configurations may be partial; username/password are optional.
	if err := ValidateSMTP(SMTP{}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSMTP(SMTP{Port: 99999, Security: "x"}); err != nil {
		t.Fatalf("disabled partial: %v", err)
	}
	if err := ValidateSMTP(SMTP{Host: "h", Port: 25, Security: "none", From: "a@b.com", Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

func TestSMTPSettingRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	s, err := db.LoadSettings(ctx)
	if err != nil || s.SMTP.Enabled || s.SMTP.Password != "" {
		t.Fatalf("default: %+v %v", s.SMTP, err)
	}
	cfg := SMTP{Host: "h", Port: 465, Security: "tls", Username: "u", Password: "p", From: "a@b.com", Enabled: true}
	if err := db.SetSettingJSON(ctx, SettingSMTP, cfg); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSettingJSON(ctx, SettingSMTP, SMTP{Enabled: true}); err == nil {
		t.Fatal("invalid enabled config stored")
	}
	s, err = db.LoadSettings(ctx)
	if err != nil || s.SMTP != cfg {
		t.Fatalf("loaded: %+v %v", s.SMTP, err)
	}
	// SaveSettings carries it along unchanged.
	if err := db.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if s2, _ := db.LoadSettings(ctx); s2.SMTP != cfg {
		t.Fatalf("after save: %+v", s2.SMTP)
	}
}

func TestTaskNotifyEmailColumn(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateTask(ctx, Task{ID: "a", Name: "A", Type: "blobstore.compact", Schedule: "manual", Params: "{}", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetTask(ctx, "a")
	if got.NotifyEmail == nil || len(got.NotifyEmail) != 0 {
		t.Fatalf("default %#v", got.NotifyEmail)
	}
	got.NotifyEmail = []string{"x@example.com", "Y <y@example.com>"}
	if err := db.UpdateTask(ctx, got); err != nil {
		t.Fatal(err)
	}
	list, _ := db.ListTasks(ctx)
	if len(list) != 1 || len(list[0].NotifyEmail) != 2 || list[0].NotifyEmail[1] != "Y <y@example.com>" {
		t.Fatalf("list %+v", list)
	}
}
