package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	maxEventStreams = 64
	eventHeartbeat  = 10 * time.Second
	eventWriteLimit = 2 * time.Second
)

type eventStream struct {
	tokenHash string
	site      *string
	versions  chan uint64
	cancel    context.CancelFunc
}

// 版本僅在本程序內有效。場域各自累計，總部涵蓋所有場域。
type liveEvents struct {
	mu          sync.Mutex
	stopped     bool
	streams     map[*eventStream]struct{}
	siteVersion map[string]uint64
	allVersion  uint64
}

// subscribe 登記 Cookie 對應的連線並取得同一時間點的範圍版本。
func (e *liveEvents) subscribe(hash []byte, site *string, cancel context.CancelFunc) (*eventStream, uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped || len(e.streams) >= maxEventStreams {
		return nil, 0
	}
	if e.streams == nil {
		e.streams = make(map[*eventStream]struct{})
	}
	stream := &eventStream{string(hash), site, make(chan uint64, 1), cancel}
	e.streams[stream] = struct{}{}
	if site != nil {
		return stream, e.siteVersion[*site]
	}
	return stream, e.allVersion
}

// unsubscribe 在處理端確實離開後歸還槽位，包含網路寫入逾時的情況。
func (e *liveEvents) unsubscribe(stream *eventStream) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.streams, stream)
}

// publish 在資料庫序列化操作內提交後呼叫。每次提交只推進各受影響範圍一次。
func (e *liveEvents) publish(sites ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped || len(sites) == 0 {
		return
	}
	if e.siteVersion == nil {
		e.siteVersion = make(map[string]uint64)
	}
	e.allVersion++
	for _, site := range sites {
		e.siteVersion[site]++
	}
	for stream := range e.streams {
		version := e.allVersion
		if stream.site != nil {
			if !slices.Contains(sites, *stream.site) {
				continue
			}
			version = e.siteVersion[*stream.site]
		}
		// 單一發送者持鎖，先捨棄舊通知即可寫入最新版本，無需等待讀取端。
		select {
		case <-stream.versions:
		default:
		}
		stream.versions <- version
	}
}

// revoke 停止與撤銷 Cookie 相同的連線。呼叫者仍持有資料庫操作槽位。
func (e *liveEvents) revoke(hash []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for stream := range e.streams {
		if stream.tokenHash == string(hash) {
			stream.cancel()
		}
	}
}

// stop 在正常關閉前停止所有串流並拒絕新訂閱，讓 HTTP 可完成關閉。
func (e *liveEvents) stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = true
	for stream := range e.streams {
		stream.cancel()
	}
}

// eventVersions 傳送已授權範圍的版本及 session 資訊，快照由各授權讀取端點提供。
func (a *application) eventVersions(c *gin.Context) {
	if !noQuery(c) || !emptyBody(c) {
		return
	}
	r := c.Request
	if len(r.Header.Values("Origin")) != 0 && !singleHeader(r, "Origin", a.origin) {
		reject(c, http.StatusForbidden, "forbidden")
		return
	}
	hash := sessionHash(r)
	if hash == nil {
		reject(c, http.StatusUnauthorized, "unauthenticated")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var stream *eventStream
	var version uint64
	var expiresAt time.Time
	// 驗證及登記與資料提交、撤銷共用順序，避免先讀後訂閱漏掉變動。
	err := a.db.use(ctx, func(wait context.Context, conn *sql.Conn) error {
		var site *string
		err := conn.QueryRowContext(wait, "SELECT a.site_id, s.expires_at"+sessionSourceSQL, hash).
			Scan(&site, &expiresAt)
		if err != nil {
			return err
		}
		stream, version = a.events.subscribe(hash, site, cancel)
		return nil
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		reject(c, http.StatusUnauthorized, "unauthenticated")
		return
	case err != nil || stream == nil:
		unavailable(c)
		return
	}
	defer a.events.unsubscribe(stream)

	// Gin 的 Flush 不回傳底層錯誤。用它的 Unwrap 取得原始 writer，再檢查每次 flush。
	writer, ok := c.Writer.(interface{ Unwrap() http.ResponseWriter })
	if !ok {
		unavailable(c)
		return
	}
	controller := http.NewResponseController(writer.Unwrap())
	// 先保留伺服器初始寫入上限，開始串流後改用每個訊框自己的期限。
	if err := controller.SetWriteDeadline(time.Now().Add(eventWriteLimit)); err != nil {
		unavailable(c)
		return
	}
	c.Header("Content-Type", "text/event-stream")
	streamVersions(ctx, c.Writer, controller, stream.versions, version, sessionViewer(hash), expiresAt)
}

// streamVersions 在首次通知後等待更新或心跳，任何終止訊號都停止讀取待送版本。
func streamVersions(ctx context.Context, writer io.Writer, controller *http.ResponseController, versions <-chan uint64, version uint64, viewer string, expiresAt time.Time) {
	expiry := time.NewTimer(time.Until(expiresAt))
	defer expiry.Stop()
	heartbeat := time.NewTicker(eventHeartbeat)
	defer heartbeat.Stop()
	expiryText := expiresAt.UTC().Format(time.RFC3339Nano)
	frame := versionFrame(version, viewer, expiryText)
	for {
		now := time.Now()
		if ctx.Err() != nil || !now.Before(expiresAt) {
			return
		}
		// 網路寫入不持有資料庫或訂閱鎖。寫完清除期限，留待下一個訊框重新設定。
		if err := controller.SetWriteDeadline(now.Add(min(eventWriteLimit, expiresAt.Sub(now)))); err != nil {
			return
		}
		if _, err := io.WriteString(writer, frame); err != nil {
			return
		}
		if err := controller.Flush(); err != nil {
			return
		}
		if err := controller.SetWriteDeadline(time.Time{}); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-expiry.C:
			return
		case version := <-versions:
			frame = versionFrame(version, viewer, expiryText)
		case <-heartbeat.C:
			frame = "event: heartbeat\ndata: {}\n\n"
		}
	}
}

// versionFrame 使用十進位字串，避免瀏覽器的 JSON 數字精度限制。
func versionFrame(version uint64, viewer, expiry string) string {
	return fmt.Sprintf("event: version\ndata: {\"version\":\"%d\",\"viewer\":\"%s\",\"expiresAt\":\"%s\"}\n\n", version, viewer, expiry)
}
