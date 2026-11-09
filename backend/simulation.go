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

func newSimulator(ctx context.Context, submit func(context.Context, Measurement) (bool, error)) *simulator {
	ctx, cancel := context.WithCancel(ctx)
	return &simulator{ctx: ctx, cancel: cancel, submit: submit, devices: make(map[string]*simulatedDevice)}
}

func (s *simulator) close() {
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()
	s.workers.Wait()
}

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
	s.workers.Go(func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case tick := <-ticker.C:
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

const simulationStateSQL = `
	SELECT COALESCE(s.mode, 'stopped'), COALESCE(s.version, 0)
	FROM devices d LEFT JOIN device_simulations s ON s.device_id = d.id
	WHERE d.id = $1
`

func simulationActor(row rowScanner) (string, error) {
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

func (a *application) readSimulation(ctx context.Context, id string, hash []byte) (state simulationState, err error) {
	s := a.simulator
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := simulationActor(conn.QueryRowContext(ctx, sessionIdentitySQL, hash)); err != nil {
			return err
		}
		if !validDeviceID(id) {
			return sql.ErrNoRows
		}
		var version int64
		if err := conn.QueryRowContext(ctx, simulationStateSQL, id).Scan(&state.Mode, &version); err != nil {
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
		actor, err := simulationActor(tx.QueryRowContext(ctx, sessionIdentitySQL, hash))
		if err != nil {
			return err
		}
		if !validDeviceID(id) {
			return sql.ErrNoRows
		}
		var mode string
		var version int64
		if err := tx.QueryRowContext(ctx, simulationStateSQL, id).Scan(&mode, &version); err != nil {
			return err
		}
		if version != expected {
			return errSimulationConflict
		}
		if version == math.MaxInt64 {
			return errors.New("simulation control version exhausted")
		}

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
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO simulation_history (device_id, version, actor, action, occurred_at_ns) VALUES ($1, $2, $3, $4, $5)
		`, id, next, actor, action, nowNS); err != nil {
			return fmt.Errorf("write simulation history: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit simulation control: %w", err)
		}

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
		return nil
	})
	if err == nil && outgoing != nil {
		s.deliver(s.ctx, *outgoing)
	}
	return state, err
}

func (a *application) simulationControl(c *gin.Context) {
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
		if err != nil || parsed < 0 || strconv.FormatInt(parsed, 10) != supplied ||
			(action != "normal" && action != "abnormal" && action != "stop" && action != "replay") {
			reject(c, http.StatusBadRequest, "invalid_input")
			return
		}
		version = parsed
	}
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
