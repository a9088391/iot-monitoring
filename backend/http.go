package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const (
	bodyLimit       = 16 * 1024
	sessionCookie   = "iot_session"
	sessionLifetime = 8 * time.Hour
)

type application struct {
	host, origin   string
	db             *lockedDB
	hashSlots      chan struct{}
	measurementNow func() time.Time
}

func (a *application) router() http.Handler {
	router := gin.New()
	router.RedirectTrailingSlash = false
	router.Use(func(c *gin.Context) {
		api := c.Request.URL.Path == "/api" || strings.HasPrefix(c.Request.URL.Path, "/api/")
		if api {
			c.Header("Cache-Control", "no-store")
		}
		if c.Request.Host != a.host {
			reject(c, http.StatusForbidden, "forbidden")
			return
		}
		defer func() {
			if recover() != nil {
				log.Print("HTTP handler failed")
				if c.Writer.Written() {
					panic(http.ErrAbortHandler)
				}
				unavailable(c)
			}
		}()
		c.Next()
	})
	router.GET("/healthz", a.health)
	router.GET("/", func(c *gin.Context) { c.File("web/index.html") })
	router.Static("/assets", "web/assets")
	router.NoRoute(func(c *gin.Context) { reject(c, http.StatusNotFound, "not_found") })

	api := router.Group("/api", a.protectAPI)
	api.POST("/login", a.login)
	api.POST("/logout", a.logout)
	api.GET("/session", a.session)
	api.GET("/devices", a.devices)
	api.GET("/devices/:id", a.device)
	api.GET("/alerts", a.alerts)
	api.GET("/alerts/:id", a.alert)
	return router
}

func reject(c *gin.Context, status int, code string) {
	c.AbortWithStatusJSON(status, gin.H{"error": code})
}

func unavailable(c *gin.Context) {
	reject(c, http.StatusServiceUnavailable, "service_unavailable")
}

func (a *application) protectAPI(c *gin.Context) {
	r := c.Request
	if r.Method == http.MethodPost &&
		(!singleHeader(r, "Origin", a.origin) || !singleHeader(r, "X-CSRF-Protection", "1")) {
		reject(c, http.StatusForbidden, "forbidden")
		return
	}
	if r.ContentLength > bodyLimit {
		reject(c, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	r.Body = http.MaxBytesReader(c.Writer, r.Body, bodyLimit)
}

func singleHeader(r *http.Request, name, expected string) bool {
	values := r.Header.Values(name)
	return len(values) == 1 && values[0] == expected
}

func readBody(c *gin.Context) ([]byte, bool) {
	body, err := io.ReadAll(c.Request.Body)
	if err == nil {
		return body, true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		reject(c, http.StatusRequestEntityTooLarge, "body_too_large")
	} else {
		reject(c, http.StatusBadRequest, "invalid_input")
	}
	return nil, false
}

func emptyBody(c *gin.Context) bool {
	body, ok := readBody(c)
	if ok && len(body) != 0 {
		reject(c, http.StatusBadRequest, "invalid_input")
		return false
	}
	return ok
}

func noQuery(c *gin.Context) bool {
	if c.Request.URL.RawQuery != "" {
		reject(c, http.StatusBadRequest, "invalid_input")
		return false
	}
	return true
}

type loginInput struct {
	username, password string
}

func parseLogin(body []byte) (loginInput, bool) {
	var input loginInput
	if !utf8.Valid(body) {
		return input, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return input, false
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return input, false
		}
		var field *string
		switch key {
		case "username":
			field = &input.username
		case "password":
			field = &input.password
		default:
			return input, false
		}
		if *field != "" {
			return input, false
		}
		token, err := decoder.Token()
		value, ok := token.(string)
		if err != nil || !ok || value == "" {
			return input, false
		}
		*field = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return input, false
	}
	_, err := decoder.Token()
	return input, err == io.EOF && input.username != "" && input.password != ""
}

func (a *application) login(c *gin.Context) {
	if !noQuery(c) {
		return
	}
	contentTypes := c.Request.Header.Values("Content-Type")
	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || len(contentTypes) != 1 || mediaType != "application/json" ||
		(len(params) > 0 && (len(params) != 1 || !strings.EqualFold(params["charset"], "utf-8"))) ||
		len(c.Request.Header.Values("Content-Encoding")) != 0 {
		reject(c, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	body, ok := readBody(c)
	if !ok {
		return
	}
	input, ok := parseLogin(body)
	if !ok {
		reject(c, http.StatusBadRequest, "invalid_input")
		return
	}

	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	peer, addressErr := netip.ParseAddr(host)
	if err != nil || addressErr != nil {
		unavailable(c)
		return
	}
	retry, err := a.reserveLoginAttempts(c.Request.Context(), input.username, peer.Unmap())
	if err != nil {
		unavailable(c)
		return
	}
	if retry > 0 {
		c.Header("Retry-After", strconv.Itoa(retry))
		reject(c, http.StatusTooManyRequests, "rate_limited")
		return
	}
	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	default:
		unavailable(c)
		return
	}

	r := c.Request
	user, hash, err := a.account(r.Context(), input.username)
	found := err == nil
	if errors.Is(err, sql.ErrNoRows) {
		hash = dummyPasswordHash
	} else if err != nil {
		unavailable(c)
		return
	}
	if !matchesPassword(hash, input.password) || !found {
		reject(c, http.StatusUnauthorized, "invalid_credentials")
		return
	}
	token, err := a.rotateSession(r.Context(), user.Username, sessionHash(r))
	if err != nil {
		unavailable(c)
		return
	}
	setCookie(c, token, int(sessionLifetime/time.Second))
	c.JSON(http.StatusOK, user)
}

func setCookie(c *gin.Context, token string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge,
	})
}

func (a *application) logout(c *gin.Context) {
	if !noQuery(c) || !emptyBody(c) {
		return
	}
	if hash := sessionHash(c.Request); hash != nil {
		if err := a.revokeSession(c.Request.Context(), hash); err != nil {
			unavailable(c)
			return
		}
	}
	setCookie(c, "", -1)
	c.Status(http.StatusNoContent)
}

func (a *application) session(c *gin.Context) {
	if !noQuery(c) || !emptyBody(c) {
		return
	}
	if user, ok := a.authenticated(c); ok {
		c.JSON(http.StatusOK, user)
	}
}

func (a *application) authenticated(c *gin.Context) (identity, bool) {
	hash := sessionHash(c.Request)
	if hash != nil {
		user, err := a.sessionIdentity(c.Request.Context(), hash)
		if err == nil {
			return user, true
		}
		if !errors.Is(err, sql.ErrNoRows) {
			unavailable(c)
			return identity{}, false
		}
	}
	reject(c, http.StatusUnauthorized, "unauthenticated")
	return identity{}, false
}

func parsePage(raw string) (int, int, bool) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return 0, 0, false
	}
	limit, offset := 50, 0
	for key, values := range query {
		if len(values) != 1 {
			return 0, 0, false
		}
		number, err := strconv.ParseUint(values[0], 10, 31)
		if err != nil {
			return 0, 0, false
		}
		switch key {
		case "limit":
			if number < 1 || number > 100 {
				return 0, 0, false
			}
			limit = int(number)
		case "offset":
			offset = int(number)
		default:
			return 0, 0, false
		}
	}
	return limit, offset, true
}

func (a *application) devices(c *gin.Context) {
	if !emptyBody(c) {
		return
	}
	limit, offset, ok := parsePage(c.Request.URL.RawQuery)
	if !ok {
		reject(c, http.StatusBadRequest, "invalid_input")
		return
	}
	if user, ok := a.authenticated(c); ok {
		result, err := a.listDevices(c.Request.Context(), user.SiteID, limit, offset)
		if err != nil {
			unavailable(c)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func (a *application) device(c *gin.Context) {
	if !noQuery(c) || !emptyBody(c) {
		return
	}
	user, ok := a.authenticated(c)
	if !ok {
		return
	}
	id := c.Param("id")
	if len(id) > 64 || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		reject(c, http.StatusNotFound, "not_found")
		return
	}
	item, err := a.findDevice(c.Request.Context(), user.SiteID, id)
	if errors.Is(err, sql.ErrNoRows) {
		reject(c, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		unavailable(c)
		return
	}
	c.JSON(http.StatusOK, item)
}

func (a *application) alerts(c *gin.Context) {
	if !emptyBody(c) {
		return
	}
	limit, offset, ok := parsePage(c.Request.URL.RawQuery)
	if !ok {
		reject(c, http.StatusBadRequest, "invalid_input")
		return
	}
	if user, ok := a.authenticated(c); ok {
		result, err := a.listAlerts(c.Request.Context(), user.SiteID, limit, offset)
		if err != nil {
			unavailable(c)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func (a *application) alert(c *gin.Context) {
	if !noQuery(c) || !emptyBody(c) {
		return
	}
	user, ok := a.authenticated(c)
	if !ok {
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 63)
	if err != nil || id == 0 {
		reject(c, http.StatusNotFound, "not_found")
		return
	}
	item, err := a.findAlert(c.Request.Context(), user.SiteID, int64(id))
	if errors.Is(err, sql.ErrNoRows) {
		reject(c, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		unavailable(c)
		return
	}
	c.JSON(http.StatusOK, item)
}

func (a *application) health(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if err := a.ping(c.Request.Context()); err != nil {
		unavailable(c)
		return
	}
	c.Status(http.StatusNoContent)
}
