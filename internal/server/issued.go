package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// issuedPrefix starts every issued token and names its format version.
const issuedPrefix = "nxi1."

// issuedClaims is the payload of an issued token. It is signed, not
// encrypted: it holds no secrets.
type issuedClaims struct {
	Provider     string `json:"p"`
	OwnerID      string `json:"o"`
	RepositoryID string `json:"r"`
	// Namespace is empty for a token that writes the trusted tier.
	Namespace string `json:"n,omitempty"`
	// Expires is a Unix time in seconds.
	Expires int64  `json:"e"`
	RunID   string `json:"run,omitempty"`
	Actor   string `json:"a,omitempty"`
}

// mintToken returns nxi1.<base64url payload>.<base64url HMAC-SHA256>, where
// the MAC covers everything before the last dot.
func mintToken(key []byte, claims *issuedClaims) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		// issuedClaims holds only strings and an integer.
		panic("server: marshal issued token claims: " + err.Error())
	}
	signed := issuedPrefix + base64.RawURLEncoding.EncodeToString(payload)
	return signed + "." + base64.RawURLEncoding.EncodeToString(issuedMAC(key, signed))
}

// parseToken returns the claims of a token signed by any of keys. It does not
// check expiry or admission. Every key is tried, so timing does not reveal
// which key matched.
func parseToken(keys [][]byte, token string) (issuedClaims, bool) {
	signed, encodedMAC, ok := strings.CutLast(token, ".")
	if !ok || !strings.HasPrefix(signed, issuedPrefix) {
		return issuedClaims{}, false
	}
	mac, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil {
		return issuedClaims{}, false
	}
	valid := false
	for _, key := range keys {
		if hmac.Equal(mac, issuedMAC(key, signed)) {
			valid = true
		}
	}
	if !valid {
		return issuedClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(signed, issuedPrefix))
	if err != nil {
		return issuedClaims{}, false
	}
	var claims issuedClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return issuedClaims{}, false
	}
	return claims, true
}

func issuedMAC(key []byte, signed string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signed))
	return mac.Sum(nil)
}
