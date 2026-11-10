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

// hashPassword 為每個帳號產生獨立 salt，使用本案固定的 Argon2id 參數保存密碼雜湊。
func hashPassword(password string) string {
	var salt [16]byte
	rand.Read(salt[:])
	hash := argon2.IDKey([]byte(password), salt[:], 2, passwordMemory, 1, 32)
	return passwordPrefix + base64.RawStdEncoding.EncodeToString(salt[:]) + "$" + base64.RawStdEncoding.EncodeToString(hash)
}

// matchesPassword 只接受本案的雜湊格式，並以固定時間比較衍生值。
func matchesPassword(encoded, password string) bool {
	// 先驗證格式、參數與解碼長度，再交給 Argon2id。
	encoded, hasPrefix := strings.CutPrefix(encoded, passwordPrefix)
	saltText, hashText, hasHash := strings.Cut(encoded, "$")
	salt, saltErr := base64.RawStdEncoding.Strict().DecodeString(saltText)
	expected, hashErr := base64.RawStdEncoding.Strict().DecodeString(hashText)
	if !hasPrefix ||
		!hasHash ||
		saltErr != nil ||
		len(salt) != 16 ||
		hashErr != nil ||
		len(expected) != 32 {
		return false
	}
	// 使用和初始化相同的參數重算，再比較固定長度的雜湊。
	actual := argon2.IDKey([]byte(password), salt, 2, passwordMemory, 1, 32)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

// randomSecret 產生 256 位元的隨機值，以不需補位的 Base64URL 表示，供本機密碼及 session 使用。
func randomSecret() string {
	var secret [32]byte
	rand.Read(secret[:])
	return base64.RawURLEncoding.EncodeToString(secret[:])
}

// validSecret 要求完整的 32 位元組 Base64URL 表示，拒絕長度錯誤或非標準編碼。
func validSecret(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(32) {
		return false
	}
	bytes, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(bytes) == 32
}

// sessionHash 只接受唯一且格式有效的 Cookie，以雜湊查詢 session，讓資料庫無須保存可直接使用的 token。
func sessionHash(request *http.Request) []byte {
	cookies := request.CookiesNamed(sessionCookie)
	if len(cookies) != 1 || !validSecret(cookies[0].Value) {
		return nil
	}
	digest := sha256.Sum256([]byte(cookies[0].Value))
	return digest[:]
}
