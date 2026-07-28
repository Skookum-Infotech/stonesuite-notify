// Command genvapid prints a fresh VAPID (P-256) keypair as
// VAPID_PUBLIC_KEY=... / VAPID_PRIVATE_KEY=... lines, in the same
// base64url-raw format `web-push generate-vapid-keys` produces. Used by
// scripts/dev-up.sh to seed .env.local on first run. Keys must stay stable
// after that — rotating them invalidates every saved test subscription,
// same as in production (see config.Config.VAPIDPublicKey).
package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

func main() {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}

	enc := base64.RawURLEncoding
	fmt.Println("VAPID_PUBLIC_KEY=" + enc.EncodeToString(key.PublicKey().Bytes()))
	fmt.Println("VAPID_PRIVATE_KEY=" + enc.EncodeToString(key.Bytes()))
}
