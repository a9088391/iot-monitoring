package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	simulationInterval       = 5 * time.Second
	simulationDataGeneration = 1
)

var (
	errSimulationUnauthenticated = errors.New("simulation session is missing or expired")
	errSimulationForbidden       = errors.New("simulation requires headquarters")
	errSimulationConflict        = errors.New("simulation version changed")
	errSimulationNoMeasurement   = errors.New("no original simulation measurement")
)

type simulationState struct {
	Mode      string `json:"mode"`
	Version   string `json:"version"`
	CanReplay bool   `json:"canReplay"`
}

type simulator struct {
	ctx     context.Context
	cancel  context.CancelFunc
	submit  func(context.Context, Measurement) (bool, error)
	mu      sync.Mutex
	devices map[string]*simulatedDevice
	workers sync.WaitGroup
}

type simulatedDevice struct {
	latest *Measurement
	run    *simulationRun
}

type simulationRun struct {
	cancel context.CancelFunc
}

// newSimulator 將所有設備的計時工作掛在同一個服務生命週期，讓關閉時可一起取消。
func newSimulator(ctx context.Context, submit func(context.Context, Measurement) (bool, error)) *simulator {
	ctx, cancel := context.WithCancel(ctx)
	return &simulator{ctx: ctx, cancel: cancel, submit: submit, devices: make(map[string]*simulatedDevice)}
}

// close 在模擬鎖內取消工作，再等計時器退出，讓服務關閉時不留下背景回報。
func (s *simulator) close() {
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()
	s.workers.Wait()
}

// deliver 經共用測量入口送出模擬樣本，略過關閉取消造成的錯誤日誌。
func (s *simulator) deliver(ctx context.Context, sample Measurement) {
	if _, err := s.submit(ctx, sample); err != nil && ctx.Err() == nil {
		log.Print("could not submit simulated measurement")
	}
}

// startRun 在控制交易提交後啟動設備自己的週期，舊計時器不可覆蓋新執行的原始回報。
func (s *simulator) startRun(device *simulatedDevice, first Measurement) {
	ctx, cancel := context.WithCancel(s.ctx)
	run := &simulationRun{cancel: cancel}
	device.latest, device.run = &first, run
	ticker := time.NewTicker(simulationInterval)
	// 每個執行管理自己的計時器，取消或被替換時由工作本身停止。
	s.workers.Go(func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case tick := <-ticker.C:
				// 在鎖內確認仍屬目前執行，才可遞增順序及更新可重送的原始樣本。
				s.mu.Lock()
				if ctx.Err() != nil || device.run != run {
					s.mu.Unlock()
					return
				}
				sample := *device.latest
				if sample.Sequence == math.MaxInt64 {
					s.mu.Unlock()
					log.Print("simulation sequence exhausted")
					return
				}
				sample.Sequence++
				sample.GeneratedAt = tick.UTC()
				device.latest = &sample
				s.mu.Unlock()

				// 送入測量入口前釋放模擬鎖，避免與控制交易的資料庫鎖相互等待。
				s.deliver(ctx, sample)
			}
		}
	})
}

// stopSimulationsOnStartup 在啟動時一起停止各裝置的模擬並撤銷舊執行資格，避免舊回報在重啟後生效。
func stopSimulationsOnStartup(ctx context.Context, conn *sql.Conn) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return operationFailure("begin startup simulation stop", err)
	}
	defer tx.Rollback()
	// 停用舊執行與更新版本同時提交，保留首筆期待、最新測量及操作歷程。
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM measurement_runs;
		UPDATE device_simulations SET mode = 'stopped', version = version + 1 WHERE mode <> 'stopped';
	`); err != nil {
		return operationFailure("invalidate prior simulation runs", err)
	}
	return operationFailure("commit startup simulation stop", tx.Commit())
}

const simulationStateFieldsSQL = `
	COALESCE(s.mode, 'stopped'), COALESCE(s.version, 0)
	FROM devices d LEFT JOIN device_simulations s ON s.device_id = d.id
	WHERE d.id = $1
`

// simulationActor 由資料庫的有效 session 辨識操作者，只允許總部並區分未登入與權限不足。
func simulationActor(row rowScanner) (string, error) {
	// 先區分查無有效 session 與查詢故障，再確認角色。
	var actor identity
	err := row.Scan(&actor.Username, &actor.Role, &actor.SiteID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errSimulationUnauthenticated
	}
	if err != nil {
		return "", err
	}
	if actor.Role != "headquarters" {
		return "", errSimulationForbidden
	}
	return actor.Username, nil
}

// readSimulation 重新驗證總部 session 後才查控制狀態，並搭配程序內的原始樣本判斷能否重送。
func (a *application) readSimulation(ctx context.Context, id string, hash []byte) (state simulationState, err error) {
	s := a.simulator
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		// 先驗證總部身分再查設備，避免權限不足者透過控制 API 辨識設備。
		if _, err := simulationActor(conn.QueryRowContext(ctx, sessionIdentitySQL, hash)); err != nil {
			return err
		}
		if !validDeviceID(id) {
			return sql.ErrNoRows
		}
		// 讀取持久的模式和版本，再從程序內的原始樣本判斷重送是否可用。
		var version int64
		if err := conn.QueryRowContext(ctx, "SELECT"+simulationStateFieldsSQL, id).Scan(&state.Mode, &version); err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		state.Version = strconv.FormatInt(version, 10)
		device := s.devices[id]
		state.CanReplay = device != nil && device.latest != nil
		return nil
	})
	return state, err
}

// changeSimulation 以版本條件保存總部的控制操作。資料庫提交後才更換計時工作或取出重送樣本。
func (a *application) changeSimulation(request context.Context, id string, hash []byte, action string, expected int64) (state simulationState, err error) {
	s := a.simulator
	var outgoing *Measurement
	// 排入後仍檢查用戶端是否離線。開始處理後以服務生命週期提交及送出，避免斷線留下半套控制。
	err = a.db.use(s.ctx, func(ctx context.Context, conn *sql.Conn) error {
		if err := request.Err(); err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin simulation control: %w", err)
		}
		defer tx.Rollback()
		// 交易內再檢查 session、角色與目前版本，拒絕以舊畫面覆蓋新的控制。
		actor, err := simulationActor(tx.QueryRowContext(ctx, sessionIdentitySQL, hash))
		if err != nil {
			return err
		}
		if !validDeviceID(id) {
			return sql.ErrNoRows
		}
		var site, mode string
		var version int64
		if err := tx.QueryRowContext(ctx, "SELECT d.site_id,"+simulationStateFieldsSQL, id).Scan(&site, &mode, &version); err != nil {
			return err
		}
		if version != expected {
			return errSimulationConflict
		}
		if version == math.MaxInt64 {
			return errors.New("simulation control version exhausted")
		}

		// 持有模擬鎖直到提交後的記憶體更新，避免計時器在中途改動原始樣本。
		s.mu.Lock()
		defer s.mu.Unlock()
		device := s.devices[id]
		if device == nil {
			device = &simulatedDevice{}
		}
		var firstExpected *int64
		var sequence int64
		start := action == "normal" || action == "abnormal"
		nowNS := a.measurementClock().UnixNano()
		switch {
		// 新執行接續已採用及已產生樣本中較大的順序，並提供可供首次開始保存的期待時間。
		case start:
			mode, firstExpected = action, &nowNS
			if err := tx.QueryRowContext(ctx, `
				SELECT COALESCE((SELECT sequence FROM latest_measurements WHERE device_id = $1), -1)
			`, id).Scan(&sequence); err != nil {
				return fmt.Errorf("read latest adopted simulation sequence: %w", err)
			}
			if device.latest != nil {
				sequence = max(sequence, device.latest.Sequence)
			}
			if sequence == math.MaxInt64 {
				return errors.New("simulation sequence exhausted")
			}
		case action == "stop":
			mode = "stopped"
		case device.latest == nil:
			return errSimulationNoMeasurement
		}
		// 以版本條件保存模式，並保留已存在的首筆期待起點。
		next := version + 1
		result, err := tx.ExecContext(ctx, `
			INSERT INTO device_simulations (device_id, mode, version, first_expected_at_ns)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (device_id) DO UPDATE SET
				mode = excluded.mode, version = excluded.version,
				first_expected_at_ns = COALESCE(device_simulations.first_expected_at_ns, excluded.first_expected_at_ns)
			WHERE device_simulations.version = $5
		`, id, mode, next, firstExpected, version)
		if err != nil {
			return fmt.Errorf("save simulation version: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("confirm simulation version: %w", err)
		}
		if changed != 1 {
			return errSimulationConflict
		}
		// 在同一交易中啟用新執行或停用舊執行，重送則維持原執行資格。
		switch {
		case start:
			_, err = tx.ExecContext(ctx, `
				INSERT INTO measurement_runs (device_id, data_generation, run_id) VALUES ($1, $2, $3)
				ON CONFLICT (device_id) DO UPDATE SET data_generation = excluded.data_generation, run_id = excluded.run_id
			`, id, simulationDataGeneration, next)
		case action == "stop":
			_, err = tx.ExecContext(ctx, "DELETE FROM measurement_runs WHERE device_id = $1", id)
		}
		if err != nil {
			return fmt.Errorf("change allowed simulation run: %w", err)
		}
		// 與控制狀態一同保存操作歷程，提交失敗就不調整程序內的計時器。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO simulation_history (device_id, version, actor, action, occurred_at_ns) VALUES ($1, $2, $3, $4, $5)
		`, id, next, actor, action, nowNS); err != nil {
			return fmt.Errorf("write simulation history: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit simulation control: %w", err)
		}

		// 提交後才取消或啟動計時器。重送使用未改動的原始樣本及其時間。
		switch {
		case start:
			if device.run != nil {
				device.run.cancel()
			}
			temperature := 23.0
			if action == "abnormal" {
				temperature = 30
			}
			outgoing = &Measurement{
				Source: SimulatorMeasurementSource, DeviceID: id, DataGeneration: simulationDataGeneration,
				RunID: next, Sequence: sequence + 1, GeneratedAt: a.measurementClock().UTC(), Temperature: temperature,
			}
			s.devices[id] = device
			s.startRun(device, *outgoing)
		case action == "stop":
			if device.run != nil {
				device.run.cancel()
				device.run = nil
			}
		case action == "replay":
			outgoing = device.latest
		}
		state = simulationState{Mode: mode, Version: strconv.FormatInt(next, 10), CanReplay: device.latest != nil}
		a.events.publish(site)
		return nil
	})
	// 離開持鎖連線後才送第一筆或重送，讓測量入口能自行取得連線。
	if err == nil && outgoing != nil {
		s.deliver(s.ctx, *outgoing)
	}
	return state, err
}

// simulationControl 驗證控制 API 的輸入，交由資料庫路徑檢查權限，並映射各種結果。
func (a *application) simulationControl(c *gin.Context) {
	// 讀取只接受空本文，寫入只接受已知操作及可完整往返的版本字串。
	if !noQuery(c) {
		return
	}
	var action string
	var version int64
	if c.Request.Method == http.MethodGet {
		if !emptyBody(c) {
			return
		}
	} else {
		body, ok := readJSONBody(c)
		if !ok {
			return
		}
		var supplied string
		if !parseRequiredStrings(body, map[string]*string{"action": &action, "version": &supplied}) {
			reject(c, http.StatusBadRequest, "invalid_input")
			return
		}
		parsed, err := strconv.ParseInt(supplied, 10, 64)
		if err != nil ||
			parsed < 0 ||
			strconv.FormatInt(parsed, 10) != supplied ||
			(action != "normal" &&
				action != "abnormal" &&
				action != "stop" &&
				action != "replay") {
			reject(c, http.StatusBadRequest, "invalid_input")
			return
		}
		version = parsed
	}
	// 先取得可供驗證的 session 雜湊，再分流到讀取或控制交易。
	hash := sessionHash(c.Request)
	if hash == nil {
		reject(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	id, ctx := c.Param("id"), c.Request.Context()
	var state simulationState
	var err error
	if c.Request.Method == http.MethodGet {
		state, err = a.readSimulation(ctx, id, hash)
	} else {
		state, err = a.changeSimulation(ctx, id, hash, action, version)
	}
	// 保留授權、版本衝突與無樣本可重送等可辨識的回應。
	switch {
	case err == nil:
		c.JSON(http.StatusOK, state)
	case errors.Is(err, errSimulationUnauthenticated):
		reject(c, http.StatusUnauthorized, "unauthenticated")
	case errors.Is(err, errSimulationForbidden):
		reject(c, http.StatusForbidden, "forbidden")
	case errors.Is(err, sql.ErrNoRows):
		reject(c, http.StatusNotFound, "not_found")
	case errors.Is(err, errSimulationConflict):
		reject(c, http.StatusConflict, "conflict")
	case errors.Is(err, errSimulationNoMeasurement):
		reject(c, http.StatusConflict, "no_measurement")
	default:
		unavailable(c)
	}
}
