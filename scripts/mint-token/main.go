// Command mint-token mints an HS256 JWT matching the claim shape
// middleware.RequireAuth expects (id, email, tenant_id, active_role_id,
// optional permissions) — for local testing of stonesuite-notify only,
// against JWT_SECRET from the environment. Not for production use: real
// sessions are minted by StoneSuite-Backend at login.
//
// Usage:
//
//	go run ./scripts/mint-token                  # self-service token
//	go run ./scripts/mint-token --admin          # + elevated permissions
//	go run ./scripts/mint-token --perms notification:read,audit:read
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// elevatedPerms grants every permission this service checks — see
// middleware/permissions.go — so one --admin token can exercise both the
// self-service and tenant-admin routes without minting two tokens.
const elevatedPerms = "notification:read,notification:update,preference:read,preference:update,push:manage,notification:admin,preference:admin,audit:read"

func main() {
	id := flag.String("id", "22222222-2222-2222-2222-222222222222", "user id (JWT 'id' claim)")
	email := flag.String("email", "test.user@example.com", "user email")
	tenant := flag.String("tenant", "11111111-1111-1111-1111-111111111111", "tenant id")
	role := flag.String("role", "role-user", "active_role_id claim")
	perms := flag.String("perms", "", "comma-separated permissions claim; omit for the implicit self-service set")
	admin := flag.Bool("admin", false, "shortcut for the full elevated permission set (overrides --perms)")
	hours := flag.Int("hours", 24, "token lifetime in hours")
	flag.Parse()

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "mint-token: JWT_SECRET is not set — source .env.local first (see scripts/dev-up.sh)")
		os.Exit(1)
	}

	claims := map[string]any{
		"id":             *id,
		"email":          *email,
		"tenant_id":      *tenant,
		"active_role_id": *role,
		"exp":            time.Now().Add(time.Duration(*hours) * time.Hour).Unix(),
	}

	permList := *perms
	if *admin {
		permList = elevatedPerms
	}
	if permList != "" {
		claims["permissions"] = strings.Split(permList, ",")
	}

	fmt.Println(sign(secret, claims))
}

func sign(secret string, claims map[string]any) string {
	header := map[string]any{"alg": "HS256", "typ": "JWT"}
	signingInput := b64(mustJSON(header)) + "." + b64(mustJSON(claims))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + b64(mac.Sum(nil))
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func b64(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
