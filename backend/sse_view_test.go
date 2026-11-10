package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	testSessionViewHeader   = "X-IOT-Session-View"
	testSessionExpiryHeader = "X-IOT-Session-Expires-At"
)

// testSSESessionView 核對同帳號的不同登入會得到不同識別值，且識別值不能取代 Cookie。
func testSSESessionView(t *testing.T, f *measurementFixture, conn *sql.Conn, north *http.Cookie, password string) (string, *http.Cookie) {
	t.Helper()
	const northIdentity = `{"username":"north","role":"site","siteId":"north"}`
	first, body := requestWithSSEView(t, f, http.MethodGet, "/api/session", "", north, http.StatusOK)
	firstView := expectSSESessionHeaders(t, conn, first, north)
	if body != northIdentity {
		t.Fatalf("unexpected session identity JSON: %s", body)
	}
	// 以同帳號再登入，但不帶原 Cookie，讓兩個 session 同時有效。
	loginBody, err := json.Marshal(map[string]string{"username": "north", "password": password})
	if err != nil {
		t.Fatal(err)
	}
	login, body := requestWithSSEView(t, f, http.MethodPost, "/api/login", string(loginBody), nil, http.StatusOK)
	cookies := login.Cookies()
	if len(cookies) != 1 || body != northIdentity {
		t.Fatal("login did not return the existing identity JSON and one Cookie")
	}
	other := cookies[0]
	otherView := expectSSESessionHeaders(t, conn, login, other)
	if firstView == otherView {
		t.Fatal("distinct sessions shared one view")
	}
	second, _ := requestWithSSEView(t, f, http.MethodGet, "/api/session", "", other, http.StatusOK, otherView)
	if expectSSESessionHeaders(t, conn, second, other) != otherView {
		t.Fatal("login and session returned different views for one Cookie")
	}

	// 有效 Cookie 仍必須與前端指定的 session 相符，未附標頭的舊客戶端維持原契約。
	requestWithSSEView(t, f, http.MethodGet, "/api/devices", "", other, http.StatusUnauthorized, firstView)
	requestWithSSEView(t, f, http.MethodGet, "/api/devices", "", nil, http.StatusUnauthorized, firstView)
	requestWithSSEView(t, f, http.MethodGet, "/api/devices", "", other, http.StatusUnauthorized, otherView, otherView)
	requestWithSSEView(t, f, http.MethodGet, "/api/devices", "", other, http.StatusUnauthorized, "")
	requestWithSSEView(t, f, http.MethodGet, "/api/devices", "", other, http.StatusOK, otherView)
	requestWithSSEView(t, f, http.MethodPost, "/api/logout", "", other, http.StatusUnauthorized, firstView)
	f.request(t, http.MethodGet, "/api/session", "", other, http.StatusOK)
	return firstView, other
}

// expectSSESessionHeaders 核對標頭與實際 Cookie 所查得的資料庫期限，且不得直接揭露 Cookie 或資料庫雜湊。
func expectSSESessionHeaders(t *testing.T, conn *sql.Conn, res *http.Response, cookie *http.Cookie) string {
	t.Helper()
	view := res.Header.Get(testSessionViewHeader)
	digest := sessionHash(&http.Request{Header: http.Header{"Cookie": {cookie.String()}}})
	if !validSecret(view) ||
		view == cookie.Value ||
		view == base64.RawURLEncoding.EncodeToString(digest) {
		t.Fatal("invalid or unseparated session view")
	}
	if want := sseSessionExpiry(t, conn, cookie).Format(time.RFC3339Nano); res.Header.Get(testSessionExpiryHeader) != want {
		t.Fatal("session expiry header did not match persisted UTC expiry")
	}
	return view
}

// requestWithSSEView 經真實 HTTP 傳送可重複的 session 識別標頭，並核對共同的未登入回應。
func requestWithSSEView(t *testing.T, f *measurementFixture, method, path, body string, cookie *http.Cookie, want int, views ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = f.app.host
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if method == http.MethodPost {
		req.Header.Set("Origin", f.app.origin)
		req.Header.Set("X-CSRF-Protection", "1")
		req.Header.Set("Content-Type", "application/json")
	}
	for _, view := range views {
		req.Header.Add(testSessionViewHeader, view)
	}
	res, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("%s %s returned %d, want %d", method, path, res.StatusCode, want)
	}
	if want == http.StatusUnauthorized && string(data) != `{"error":"unauthenticated"}` {
		t.Fatal("session mismatch did not use the existing unauthenticated response")
	}
	return res, string(data)
}

// expectSSEHeartbeatData 確認具名心跳只帶空物件，讓瀏覽器可以監測連線而不取得業務資料。
func expectSSEHeartbeatData(t *testing.T, stream testEventStream) {
	t.Helper()
	if data, err := stream.reader.ReadString('\n'); err != nil || data != "data: {}\n" {
		t.Fatalf("expected empty heartbeat data, got %q: %v", data, err)
	}
	if end, err := stream.reader.ReadString('\n'); err != nil || end != "\n" {
		t.Fatalf("expected heartbeat terminator, got %q: %v", end, err)
	}
}
