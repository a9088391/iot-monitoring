package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMemory = 19 * 1024
	passwordPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"
)

var dummyPasswordHash = passwordPrefix +
	base64.RawStdEncoding.EncodeToString(make([]byte, 16)) + "$" +
	base64.RawStdEncoding.EncodeToString(make([]byte, 32))

func hashPassword(password string) string {
	var salt [16]byte
	rand.Read(salt[:])
	hash := argon2.IDKey([]byte(password), salt[:], 2, passwordMemory, 1, 32)
	return passwordPrefix + base64.RawStdEncoding.EncodeToString(salt[:]) + "$" + base64.RawStdEncoding.EncodeToString(hash)
}

func matchesPassword(encoded, password string) bool {
	encoded, hasPrefix := strings.CutPrefix(encoded, passwordPrefix)
	saltText, hashText, hasHash := strings.Cut(encoded, "$")
	salt, saltErr := base64.RawStdEncoding.Strict().DecodeString(saltText)
	expected, hashErr := base64.RawStdEncoding.Strict().DecodeString(hashText)
	if !hasPrefix || !hasHash || saltErr != nil || len(salt) != 16 || hashErr != nil || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 2, passwordMemory, 1, 32)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func randomSecret() string {
	var secret [32]byte
	rand.Read(secret[:])
	return base64.RawURLEncoding.EncodeToString(secret[:])
}

func validSecret(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(32) {
		return false
	}
	bytes, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(bytes) == 32
}

func sessionHash(request *http.Request) []byte {
	cookies := request.CookiesNamed(sessionCookie)
	if len(cookies) != 1 || !validSecret(cookies[0].Value) {
		return nil
	}
	digest := sha256.Sum256([]byte(cookies[0].Value))
	return digest[:]
}
