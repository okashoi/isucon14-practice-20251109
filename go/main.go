package main

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
    "github.com/kaz/pprotein/integration"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

var db *sqlx.DB

// 通知チャネル管理
var (
	appNotificationChannels   = make(map[string]chan struct{})
	chairNotificationChannels = make(map[string]chan struct{})
	notificationMutex         sync.RWMutex
)

// chair_locations のバッファリング用
var (
	chairLocationBuffer      = []ChairLocation{}
	chairLocationBufferMutex sync.Mutex
)

// マッチングキュー
var (
	matchingQueue = make(chan string, 1000) // ride IDのキュー
)

func main() {
	mux := setup()
	slog.Info("Listening on :8080")
	http.ListenAndServe(":8080", mux)
}

func setup() http.Handler {
	host := os.Getenv("ISUCON_DB_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("ISUCON_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		panic(fmt.Sprintf("failed to convert DB port number from ISUCON_DB_PORT environment variable into int: %v", err))
	}
	user := os.Getenv("ISUCON_DB_USER")
	if user == "" {
		user = "isucon"
	}
	password := os.Getenv("ISUCON_DB_PASSWORD")
	if password == "" {
		password = "isucon"
	}
	dbname := os.Getenv("ISUCON_DB_NAME")
	if dbname == "" {
		dbname = "isuride"
	}

	dbConfig := mysql.NewConfig()
	dbConfig.User = user
	dbConfig.Passwd = password
	dbConfig.Addr = net.JoinHostPort(host, port)
	dbConfig.Net = "tcp"
	dbConfig.DBName = dbname
	dbConfig.ParseTime = true
	dbConfig.InterpolateParams = true

	_db, err := sqlx.Connect("mysql", dbConfig.FormatDSN())
	if err != nil {
		panic(err)
	}
	db = _db
	db.SetMaxOpenConns(100)
	db.SetMaxIdleConns(100)

	mux := chi.NewRouter()
	mux.Use(middleware.Logger)
	mux.Use(middleware.Recoverer)
	mux.HandleFunc("POST /api/initialize", postInitialize)

	// app handlers
	{
		mux.HandleFunc("POST /api/app/users", appPostUsers)

		authedMux := mux.With(appAuthMiddleware)
		authedMux.HandleFunc("POST /api/app/payment-methods", appPostPaymentMethods)
		authedMux.HandleFunc("GET /api/app/rides", appGetRides)
		authedMux.HandleFunc("POST /api/app/rides", appPostRides)
		authedMux.HandleFunc("POST /api/app/rides/estimated-fare", appPostRidesEstimatedFare)
		authedMux.HandleFunc("POST /api/app/rides/{ride_id}/evaluation", appPostRideEvaluatation)
		authedMux.HandleFunc("GET /api/app/notification", appGetNotification)
		authedMux.HandleFunc("GET /api/app/nearby-chairs", appGetNearbyChairs)
	}

	// owner handlers
	{
		mux.HandleFunc("POST /api/owner/owners", ownerPostOwners)

		authedMux := mux.With(ownerAuthMiddleware)
		authedMux.HandleFunc("GET /api/owner/sales", ownerGetSales)
		authedMux.HandleFunc("GET /api/owner/chairs", ownerGetChairs)
	}

	// chair handlers
	{
		mux.HandleFunc("POST /api/chair/chairs", chairPostChairs)

		authedMux := mux.With(chairAuthMiddleware)
		authedMux.HandleFunc("POST /api/chair/activity", chairPostActivity)
		authedMux.HandleFunc("POST /api/chair/coordinate", chairPostCoordinate)
		authedMux.HandleFunc("GET /api/chair/notification", chairGetNotification)
		authedMux.HandleFunc("POST /api/chair/rides/{ride_id}/status", chairPostRideStatus)
	}

	// internal handlers
	{
		mux.HandleFunc("GET /api/internal/matching", internalGetMatching)
	}

	pproteinHandler := integration.NewDebugHandler()
	go http.ListenAndServe(":3000", pproteinHandler)

	// chair_locations のバルクインサート用goroutineを起動
	go bulkInsertChairLocations()

	// マッチングワーカーgoroutineを起動
	go matchingWorker()

	return mux
}

type postInitializeRequest struct {
	PaymentServer string `json:"payment_server"`
}

type postInitializeResponse struct {
	Language string `json:"language"`
}

func postInitialize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := &postInitializeRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if out, err := exec.Command("../sql/init.sh").CombinedOutput(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to initialize: %s: %w", string(out), err))
		return
	}

	if _, err := db.ExecContext(ctx, "UPDATE settings SET value = ? WHERE name = 'payment_gateway_url'", req.PaymentServer); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 通知チャネルをクリア
	notificationMutex.Lock()
	appNotificationChannels = make(map[string]chan struct{})
	chairNotificationChannels = make(map[string]chan struct{})
	notificationMutex.Unlock()

	// chair_locationsバッファをクリア
	chairLocationBufferMutex.Lock()
	chairLocationBuffer = []ChairLocation{}
	chairLocationBufferMutex.Unlock()

	// マッチングキューをクリア（既存のキューを消費）
	for {
		select {
		case <-matchingQueue:
			// キューから全て取り出す
		default:
			// キューが空になったら終了
			goto QUEUE_CLEARED
		}
	}
QUEUE_CLEARED:

	go func() {
		if _, err := http.Get("http://54.238.146.225:9000/api/group/collect"); err != nil {
			//log.Printf("failed to communicate with pprotein: %v", err)
		}
	}()

	writeJSON(w, http.StatusOK, postInitializeResponse{Language: "go"})
}

type Coordinate struct {
	Latitude  int `json:"latitude"`
	Longitude int `json:"longitude"`
}

func bindJSON(r *http.Request, v interface{}) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func writeJSON(w http.ResponseWriter, statusCode int, v interface{}) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	buf, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(statusCode)
	w.Write(buf)
}

func writeError(w http.ResponseWriter, statusCode int, err error) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(statusCode)
	buf, marshalError := json.Marshal(map[string]string{"message": err.Error()})
	if marshalError != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"marshaling error failed"}`))
		return
	}
	w.Write(buf)

	slog.Error("error response wrote", err)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

// chair_locations のバルクインサート処理
func bulkInsertChairLocations() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		chairLocationBufferMutex.Lock()
		if len(chairLocationBuffer) == 0 {
			chairLocationBufferMutex.Unlock()
			continue
		}

		// バッファをコピーして即座にクリア
		locations := make([]ChairLocation, len(chairLocationBuffer))
		copy(locations, chairLocationBuffer)
		chairLocationBuffer = chairLocationBuffer[:0]
		chairLocationBufferMutex.Unlock()

		// バルクインサート実行
		if len(locations) > 0 {
			insertChairLocationsBulk(locations)
		}
	}
}

func insertChairLocationsBulk(locations []ChairLocation) {
	if len(locations) == 0 {
		return
	}

	// sqlx.NamedExecを使ってバルクインサート
	query := `INSERT INTO chair_locations (id, chair_id, latitude, longitude, created_at) VALUES (:id, :chair_id, :latitude, :longitude, :created_at)`
	_, err := db.NamedExec(query, locations)
	if err != nil {
		slog.Error("bulk insert chair_locations failed", "error", err, "count", len(locations))
	}
}

// マッチングワーカー
func matchingWorker() {
	for rideID := range matchingQueue {
		tryMatchRide(rideID)
		// 過負荷防止のため少し待機
		time.Sleep(10 * time.Millisecond)
	}
}

// 1つのrideに対してマッチング処理を試行
func tryMatchRide(rideID string) {
	ctx := context.Background()

	// rideの情報を取得
	ride := &Ride{}
	if err := db.GetContext(ctx, ride, `SELECT * FROM rides WHERE id = ? AND chair_id IS NULL`, rideID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 既にマッチング済み、またはキャンセル済み
			return
		}
		slog.Error("failed to get ride", "error", err, "ride_id", rideID)
		return
	}

	// pickup座標に近い空いている椅子を取得してアサイン
	matched := &Chair{}
	query := `
		SELECT c.*
		FROM chairs c
		WHERE c.is_active = TRUE
		AND c.latest_latitude IS NOT NULL
		AND c.latest_longitude IS NOT NULL
		AND NOT EXISTS (
			SELECT 1
			FROM rides r
			WHERE r.chair_id = c.id
			AND EXISTS (
				SELECT 1
				FROM ride_statuses rs
				WHERE rs.ride_id = r.id
				GROUP BY rs.ride_id
				HAVING COUNT(rs.chair_sent_at) < 6
			)
		)
		ORDER BY
			(c.latest_latitude - ?) * (c.latest_latitude - ?) +
			(c.latest_longitude - ?) * (c.latest_longitude - ?)
		LIMIT 1
	`
	if err := db.GetContext(ctx, matched, query,
		ride.PickupLatitude, ride.PickupLatitude,
		ride.PickupLongitude, ride.PickupLongitude,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 空いている椅子が見つからない場合は、キューに再投入
			select {
			case matchingQueue <- rideID:
			default:
				// キューが満杯の場合はスキップ
			}
			return
		}
		slog.Error("failed to find available chair", "error", err, "ride_id", rideID)
		return
	}

	// 空いている椅子が見つかったのでアサイン
	if _, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ? AND chair_id IS NULL", matched.ID, ride.ID); err != nil {
		slog.Error("failed to assign chair to ride", "error", err, "ride_id", rideID, "chair_id", matched.ID)
		return
	}

	// マッチング成立を即座に通知
	notificationMutex.RLock()
	if ch, ok := appNotificationChannels[ride.UserID]; ok {
		select {
		case ch <- struct{}{}:
		default: // ブロッキング回避
		}
	}
	if ch, ok := chairNotificationChannels[matched.ID]; ok {
		select {
		case ch <- struct{}{}:
		default: // ブロッキング回避
		}
	}
	notificationMutex.RUnlock()
}
