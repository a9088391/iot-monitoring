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
	arrivals       pendingMeasurements
	simulator      *simulator
}

// router 組裝本機網站與 API，讓共同的 Host 檢查及 API 寫入保護先於各端點執行。
func (a *application) router() http.Handler {
	router := gin.New()
	router.RedirectTrailingSlash = false
	// 所有路徑都驗證 Host，API 回應額外禁止快取，以免重用登入或場域資料。
	router.Use(func(c *gin.Context) {
		api := c.Request.URL.Path == "/api" || strings.HasPrefix(c.Request.URL.Path, "/api/")
		if api {
			c.Header("Cache-Control", "no-store")
		}
		if c.Request.Host != a.host {
			reject(c, http.StatusForbidden, "forbidden")
			return
		}
		// 處理端發生 panic 時只記錄通用訊息。已開始回應就中止連線，避免送出混合回應。
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
	// 掛上健康檢查與靜態頁面，統一未匹配路徑的錯誤格式。
	router.GET("/healthz", a.health)
	router.GET("/", func(c *gin.Context) { c.File("web/index.html") })
	router.Static("/assets", "web/assets")
	router.NoRoute(func(c *gin.Context) { reject(c, http.StatusNotFound, "not_found") })

	// 在 API 群組先套用寫入保護和本文限制，再登記各端點。
	api := router.Group("/api", a.protectAPI)
	api.POST("/login", a.login)
	api.POST("/logout", a.logout)
	api.GET("/session", a.session)
	api.GET("/devices", a.devices)
	api.GET("/devices/:id", a.device)
	api.GET("/devices/:id/simulation", a.simulationControl)
	api.POST("/devices/:id/simulation", a.simulationControl)
	api.GET("/alerts", a.alerts)
	api.GET("/alerts/:id", a.alert)
	api.POST("/alerts/:id/close", a.alertClose)
	return router
}

// reject 中止目前的處理鏈，統一以機器可辨識的錯誤代碼回應。
func reject(c *gin.Context, status int, code string) {
	c.AbortWithStatusJSON(status, gin.H{"error": code})
}

// unavailable 以固定代碼回報暫時無法處理的請求，避免把內部錯誤送給用戶端。
func unavailable(c *gin.Context) {
	reject(c, http.StatusServiceUnavailable, "service_unavailable")
}

// protectAPI 對瀏覽器寫入檢查 CSRF，並在各端點讀取前限制本文大小。
func (a *application) protectAPI(c *gin.Context) {
	// 寫入必須同時帶有完全相符的 Origin 與專用標頭，登入也適用。
	r := c.Request
	if r.Method == http.MethodPost &&
		(!singleHeader(r, "Origin", a.origin) ||
			!singleHeader(r, "X-CSRF-Protection", "1")) {
		reject(c, http.StatusForbidden, "forbidden")
		return
	}
	// 已知超限可立即拒絕。其餘仍限制實際讀取，涵蓋未知長度的本文。
	if r.ContentLength > bodyLimit {
		reject(c, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	r.Body = http.MaxBytesReader(c.Writer, r.Body, bodyLimit)
}

// singleHeader 要求安全標頭只有一個且值完全相符，避免接受重複或含糊的輸入。
func singleHeader(r *http.Request, name, expected string) bool {
	values := r.Header.Values(name)
	return len(values) == 1 && values[0] == expected
}

// readBody 讀取已套上大小限制的本文，區分超過上限及其他讀取錯誤。
func readBody(c *gin.Context) ([]byte, bool) {
	body, err := io.ReadAll(c.Request.Body)
	if err == nil {
		return body, true
	}
	// 保留超過限制的專用錯誤碼，其餘讀取失敗視為輸入無效。
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		reject(c, http.StatusRequestEntityTooLarge, "body_too_large")
	} else {
		reject(c, http.StatusBadRequest, "invalid_input")
	}
	return nil, false
}

// emptyBody 拒絕未定義的本文，並沿用共同讀取函式的錯誤回應。
func emptyBody(c *gin.Context) bool {
	body, ok := readBody(c)
	if ok && len(body) != 0 {
		reject(c, http.StatusBadRequest, "invalid_input")
		return false
	}
	return ok
}

// noQuery 拒絕不接受查詢參數的端點收到額外輸入。
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

// parseLogin 將嚴格的字串欄位驗證套用到登入所需的帳號和密碼。
func parseLogin(body []byte) (loginInput, bool) {
	var input loginInput
	ok := parseRequiredStrings(body, map[string]*string{"username": &input.username, "password": &input.password})
	return input, ok
}

// parseRequiredStrings 逐一檢查 JSON 欄位，避免一般解碼覆蓋重複鍵或忽略未定義欄位。
func parseRequiredStrings(body []byte, fields map[string]*string) bool {
	// 先驗證位元組編碼及最外層物件，避免解碼器替換非法 UTF-8。
	if !utf8.Valid(body) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	// 逐欄接受允許的非空字串。欄位一旦寫入即拒絕同名鍵。
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		field := fields[key]
		if !ok ||
			field == nil ||
			*field != "" {
			return false
		}
		token, err = decoder.Token()
		value, ok := token.(string)
		if err != nil ||
			!ok ||
			value == "" {
			return false
		}
		*field = value
	}
	// 確認物件完整、後面沒有其他值，並核對每個必填欄位都出現。
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return false
	}
	_, err := decoder.Token()
	if err != io.EOF {
		return false
	}
	for _, field := range fields {
		if *field == "" {
			return false
		}
	}
	return true
}

// readJSONBody 只讀取約定的 UTF-8 JSON 本文，避免以不支援的格式解碼請求。
func readJSONBody(c *gin.Context) ([]byte, bool) {
	contentTypes := c.Request.Header.Values("Content-Type")
	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil ||
		len(contentTypes) != 1 ||
		mediaType != "application/json" ||
		(len(params) > 0 &&
			(len(params) != 1 ||
				!strings.EqualFold(params["charset"], "utf-8"))) ||
		len(c.Request.Header.Values("Content-Encoding")) != 0 {
		reject(c, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return nil, false
	}
	return readBody(c)
}

// login 先驗證輸入及保留限流額度，再驗證身分並換發 session。
func (a *application) login(c *gin.Context) {
	// 先驗證登入本文，格式錯誤不進入配額及密碼驗證流程。
	if !noQuery(c) {
		return
	}
	body, ok := readJSONBody(c)
	if !ok {
		return
	}
	input, ok := parseLogin(body)
	if !ok {
		reject(c, http.StatusBadRequest, "invalid_input")
		return
	}

	// 以連線對端計算來源額度，避免讓可由請求提供的轉送標頭決定來源。
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
	// 限制同時執行的 Argon2id 工作，結束時歸還槽位，避免擠滿記憶體。
	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	default:
		unavailable(c)
		return
	}

	// 未知帳號也驗證替代雜湊，並與密碼錯誤共用回應。
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
	// 換發成功後才通知瀏覽器，以免 Cookie 指向未建立的 session。
	token, err := a.rotateSession(r.Context(), user.Username, sessionHash(r))
	if err != nil {
		unavailable(c)
		return
	}
	setCookie(c, token, int(sessionLifetime/time.Second))
	c.JSON(http.StatusOK, user)
}

// setCookie 以限定主機、禁止前端腳本讀取且 SameSite 嚴格的 Cookie 傳送或移除 session token。
func setCookie(c *gin.Context, token string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge,
	})
}

// logout 先撤銷伺服器端 session 再清除 Cookie，使既有 token 立即失效。
func (a *application) logout(c *gin.Context) {
	if !noQuery(c) || !emptyBody(c) {
		return
	}
	// 有效格式的舊 token 先從資料庫撤銷，成功後再通知瀏覽器清除。
	if hash := sessionHash(c.Request); hash != nil {
		if err := a.revokeSession(c.Request.Context(), hash); err != nil {
			unavailable(c)
			return
		}
	}
	setCookie(c, "", -1)
	c.Status(http.StatusNoContent)
}

// session 由伺服器讀回目前登入身分，讓前端確認 session 仍有效。
func (a *application) session(c *gin.Context) {
	if !noQuery(c) || !emptyBody(c) {
		return
	}
	if user, ok := a.authenticated(c); ok {
		c.JSON(http.StatusOK, user)
	}
}

// authenticated 只從有效 session 取得身分，並區分登入失效與資料庫故障。
func (a *application) authenticated(c *gin.Context) (identity, bool) {
	hash := sessionHash(c.Request)
	// 由 token 雜湊查回身分。找不到或已到期走共同未登入回應，其他錯誤走服務不可用。
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

// parsePage 提供清單共用的分頁預設值，拒絕重複、超出範圍或未定義的參數。
func parsePage(raw string) (int, int, bool) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return 0, 0, false
	}
	// 先使用預設分頁，只有允許且單值的欄位可以覆寫。
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

// devices 依登入身分提供設備清單，場域範圍由後端決定。
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
		// 以 session 的場域限制清單，總部身分的空場域代表所有場域。
		result, err := a.listDevices(c.Request.Context(), user.SiteID, limit, offset)
		if err != nil {
			unavailable(c)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// device 依登入身分查單台設備，越權與查無資料使用相同回應。
func (a *application) device(c *gin.Context) {
	if !noQuery(c) || !emptyBody(c) {
		return
	}
	user, ok := a.authenticated(c)
	if !ok {
		return
	}
	id := c.Param("id")
	if !validDeviceID(id) {
		reject(c, http.StatusNotFound, "not_found")
		return
	}
	// 查詢同時套用設備識別碼與授權場域，避免洩漏跨場域設備。
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

// alerts 依登入身分提供告警清單，沿用與設備清單一致的分頁契約。
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
		// 以 session 的場域同時限制結果與總筆數。
		result, err := a.listAlerts(c.Request.Context(), user.SiteID, limit, offset)
		if err != nil {
			unavailable(c)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// alert 依登入身分查單筆告警，越權與查無資料使用相同回應。
func (a *application) alert(c *gin.Context) {
	if !noQuery(c) || !emptyBody(c) {
		return
	}
	user, ok := a.authenticated(c)
	if !ok {
		return
	}
	id := parseAlertID(c.Param("id"))
	if id == 0 {
		reject(c, http.StatusNotFound, "not_found")
		return
	}
	// 以場域與 ID 一起查詢，維持查無及越權一致的回應。
	item, err := a.findAlert(c.Request.Context(), user.SiteID, id)
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

// health 探測持鎖資料庫連線，禁止快取以免舊的成功回應掩蓋斷線。
func (a *application) health(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if err := a.ping(c.Request.Context()); err != nil {
		unavailable(c)
		return
	}
	c.Status(http.StatusNoContent)
}
