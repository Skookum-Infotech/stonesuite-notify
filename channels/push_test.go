package channels

import (
	"testing"

	"stonesuite-notify/config"
)

func TestSendWebPush_NotConfigured_ReturnsError(t *testing.T) {
	_, err := SendWebPush(config.Config{}, PushSubscription{
		Endpoint: "https://push.example.com/ep1",
		P256dh:   "p256dh",
		Auth:     "auth",
	}, "title", "body", "")
	if err == nil {
		t.Fatal("expected an error when VAPID keys are not configured")
	}
}
