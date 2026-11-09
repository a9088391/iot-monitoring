package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.url, strings.NewReader(test.body))
			if test.name == "oversized chunked body" {
				request.ContentLength = -1
				request.TransferEncoding = []string{"chunked"}
			}
			request.Header.Set("Origin", test.origin)
			request.Header.Set("X-CSRF-Protection", test.marker)
			request.Header.Set("Content-Type", test.mediaType)
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
