package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type identity struct {
	userID   int64
	username string
}

type jwtHeader struct {
	Alg string `json:"alg"`
}
type jwtClaims struct {
	Sub      any      `json:"sub"`
	Username any      `json:"username"`
	Exp      *float64 `json:"exp"`
	Nbf      *float64 `json:"nbf"`
}

var errToken = errors.New("invalid or expired token")
var errPayload = errors.New("invalid token payload")

// authenticate verifies HS256 signature and exp on every call (no caching).
func authenticate(header string, secret []byte) (identity, error) {
	if !strings.HasPrefix(header, "Bearer ") {
		return identity{}, errors.New("missing bearer token")
	}
	tok := header[7:]
	i := strings.IndexByte(tok, '.')
	if i < 0 {
		return identity{}, errToken
	}
	j := strings.IndexByte(tok[i+1:], '.')
	if j < 0 {
		return identity{}, errToken
	}
	j += i + 1
	if strings.IndexByte(tok[j+1:], '.') >= 0 {
		return identity{}, errToken
	}
	enc := base64.RawURLEncoding
	var sig [32]byte
	if enc.DecodedLen(len(tok)-j-1) != 32 {
		return identity{}, errToken
	}
	if n, err := enc.Decode(sig[:], []byte(tok[j+1:])); err != nil || n != 32 {
		return identity{}, errToken
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(tok[:j]))
	var sum [32]byte
	if !hmac.Equal(mac.Sum(sum[:0]), sig[:]) {
		return identity{}, errToken
	}
	hb, err := enc.DecodeString(tok[:i])
	if err != nil {
		return identity{}, errToken
	}
	var h jwtHeader
	if json.Unmarshal(hb, &h) != nil || h.Alg != "HS256" {
		return identity{}, errToken
	}
	pb, err := enc.DecodeString(tok[i+1 : j])
	if err != nil {
		return identity{}, errToken
	}
	var c jwtClaims
	if json.Unmarshal(pb, &c) != nil || c.Exp == nil {
		return identity{}, errToken
	}
	now := float64(time.Now().UnixNano()) / 1e9
	if now >= *c.Exp || (c.Nbf != nil && now < *c.Nbf) {
		return identity{}, errToken
	}
	sub, ok := c.Sub.(string)
	username, ok2 := c.Username.(string)
	if !ok || !ok2 {
		return identity{}, errPayload
	}
	id, err := parsePostID(sub)
	if err != nil {
		return identity{}, errPayload
	}
	return identity{id, username}, nil
}
