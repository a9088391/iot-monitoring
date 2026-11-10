package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLoginRejectsInvalidRequests 經實際路由驗證登入的請求保護，錯誤輸入應在接觸資料庫前被拒絕。
func TestLoginRejectsInvalidRequests(t *testing.T) {
	const origin = "http://iot.localhost:8090"
	const credentials = `{"username":"hq","password":"secret"}`

	server := (&application{host: "iot.localhost:8090", origin: origin}).router()
	tests := []struct {
		name, url, origin, marker, mediaType, body, want string
		status                                           int
	}{
		{"wrong host", "http://foreign.test/api/login", origin, "1", "application/json", credentials, "forbidden", 403},
		{"legacy IP host", "http://127.0.0.1:8090/api/login", origin, "1", "application/json", credentials, "forbidden", 403},
		{"trailing dot host", "http://iot.localhost.:8090/api/login", origin, "1", "application/json", credentials, "forbidden", 403},
		{"legacy IP origin", origin + "/api/login", "http://127.0.0.1:8090", "1", "application/json", credentials, "forbidden", 403},
		{"wrong origin port", origin + "/api/login", "http://iot.localhost:9000", "1", "application/json", credentials, "forbidden", 403},
		{"no origin", origin + "/api/login", "", "1", "application/json", credentials, "forbidden", 403},
		{"no marker", origin + "/api/login", origin, "", "application/json", credentials, "forbidden", 403},
		{"wrong media type", origin + "/api/login", origin, "1", "text/plain", credentials, "unsupported_media_type", 415},
		{"duplicate field", origin + "/api/login", origin, "1", "application/json", `{"username":"a","username":"b","password":"secret"}`, "invalid_input", 400},
		{"unknown field", origin + "/api/login", origin, "1", "application/json", `{"username":"hq","password":"secret","role":"headquarters"}`, "invalid_input", 400},
		{"wrong field case", origin + "/api/login", origin, "1", "application/json", `{"Username":"hq","password":"secret"}`, "invalid_input", 400},
		{"wrong type", origin + "/api/login", origin, "1", "application/json", `{"username":"hq","password":false}`, "invalid_input", 400},
		{"null", origin + "/api/login", origin, "1", "application/json", `{"username":"hq","password":null}`, "invalid_input", 400},
		{"missing field", origin + "/api/login", origin, "1", "application/json", `{"username":"hq"}`, "invalid_input", 400},
		{"trailing JSON", origin + "/api/login", origin, "1", "application/json", credentials + " {}", "invalid_input", 400},
		{"oversized body", origin + "/api/login", origin, "1", "application/json", strings.Repeat(" ", 16385), "body_too_large", 413},
		{"oversized chunked body", origin + "/api/login", origin, "1", "application/json", strings.Repeat(" ", 16385), "body_too_large", 413},
		{"query parameters", origin + "/api/login?role=headquarters", origin, "1", "application/json", credentials, "invalid_input", 400},
	}
	for _, test := range tests {
		// 每個案例從真實登入路由檢查拒絕原因。
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.url, strings.NewReader(test.body))
			// 將分塊案例改成未知長度，確認實際讀取也會受本文上限保護。
			if test.name == "oversized chunked body" {
				request.ContentLength = -1
				request.TransferEncoding = []string{"chunked"}
			}
			request.Header.Set("Origin", test.origin)
			request.Header.Set("X-CSRF-Protection", test.marker)
			request.Header.Set("Content-Type", test.mediaType)
			// 同時核對錯誤代碼與禁止快取標頭，避免拒絕回應被共用。
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status || response.Body.String() != `{"error":"`+test.want+`"}` {
				t.Fatalf("got %d %s, want %d %s", response.Code, response.Body, test.status, test.want)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("API response can be cached")
			}
		})
	}
}

// TestDeviceListQueryAllowlist 區分合法分頁的未登入回應與非法參數的輸入錯誤。
func TestDeviceListQueryAllowlist(t *testing.T) {
	const origin = "http://iot.localhost:8090"
	server := (&application{host: "iot.localhost:8090", origin: origin}).router()
	for _, test := range []struct {
		query  string
		status int
	}{
		{"", 401},
		{"?limit=1&offset=0", 401},
		{"?limit=100&offset=2147483647", 401},
		{"?limit=0", 400},
		{"?limit=101", 400},
		{"?limit=1.0", 400},
		{"?limit=1&limit=2", 400},
		{"?offset=-1", 400},
		{"?offset=2147483648", 400},
		{"?offset=2&offset=3", 400},
		{"?siteId=south", 400},
		{"?role=headquarters", 400},
		{"?limit=%zz", 400},
	} {
		// 逐一送出合法邊界、重複參數及非允許欄位，確認驗證發生在登入前。
		t.Run(test.query, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, origin+"/api/devices"+test.query, nil)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("got %d %s, want %d", response.Code, response.Body, test.status)
			}
		})
	}
}
