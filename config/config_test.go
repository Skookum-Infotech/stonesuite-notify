package config

import "testing"

func TestEmailSendable(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"nothing set", Config{}, false},
		{"resend key but no EMAIL_FROM", Config{ResendAPIKey: "re_x"}, false},
		{"smtp host but no EMAIL_FROM", Config{SMTPHost: "smtp.x"}, false},
		{"EMAIL_FROM but no provider", Config{EmailFrom: "a@b.com"}, false},
		{"resend key + EMAIL_FROM", Config{ResendAPIKey: "re_x", EmailFrom: "a@b.com"}, true},
		{"smtp host + EMAIL_FROM", Config{SMTPHost: "smtp.x", EmailFrom: "a@b.com"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.EmailSendable(); got != tc.want {
				t.Fatalf("EmailSendable() = %v, want %v", got, tc.want)
			}
			// EmailConfigured stays the looser "a provider key is present" check.
			if tc.cfg.EmailSendable() && !tc.cfg.EmailConfigured() {
				t.Fatal("EmailSendable implies EmailConfigured")
			}
		})
	}
}
