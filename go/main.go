package main

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/kaz/pprotein/integration"
)

var db *sqlx.DB

type notificationConnection struct {
	w       http.ResponseWriter
	flusher http.Flusher
	ctx     context.Context
}

var (
	appNotificationConnections   = make(map[string]*notificationConnection)
	chairNotificationConnections = make(map[string]*notificationConnection)
	notificationMutex            sync.RWMutex
)

// chair_locations のバッファリング用
var (
	chairLocationBuffer      = []ChairLocation{}
	chairLocationBufferMutex sync.Mutex
)

// 未送信ステータスのキャッシュ (ride_id -> []RideStatus)
var (
	appUnsentStatuses   = make(map[string][]RideStatus)
	appUnsentMutex      sync.RWMutex
	chairUnsentStatuses = make(map[string][]RideStatus)
	chairUnsentMutex    sync.RWMutex
)

// Chairキャッシュ (access_token -> *Chair)
var (
	chairCacheByAccessToken = make(map[string]*Chair)
	chairCacheMutex         sync.RWMutex
)

// 椅子のcurrent_ride_idキャッシュ (chair_id -> ride_id)
var (
	chairCurrentRideCache      = make(map[string]string) // 空文字列 = NULLを表す
	chairCurrentRideCacheMutex sync.RWMutex
)

func getChairByAccessToken(token string) (*Chair, bool) {
	chairCacheMutex.RLock()
	defer chairCacheMutex.RUnlock()
	chair, ok := chairCacheByAccessToken[token]
	return chair, ok
}

func setChairCache(token string, chair *Chair) {
	chairCacheMutex.Lock()
	defer chairCacheMutex.Unlock()
	chairCacheByAccessToken[token] = chair
}

// 椅子のcurrent_ride_idを取得（存在しない場合は空文字列を返す）
func getChairCurrentRideID(chairID string) (string, bool) {
	chairCurrentRideCacheMutex.RLock()
	defer chairCurrentRideCacheMutex.RUnlock()
	rideID, ok := chairCurrentRideCache[chairID]
	return rideID, ok
}

// 椅子のcurrent_ride_idを設定（空文字列でNULLを表す）
func setChairCurrentRideID(chairID string, rideID string) {
	chairCurrentRideCacheMutex.Lock()
	defer chairCurrentRideCacheMutex.Unlock()
	chairCurrentRideCache[chairID] = rideID
}

func addUnsentStatus(rideID string, status RideStatus) {
	appUnsentMutex.Lock()
	appUnsentStatuses[rideID] = append(appUnsentStatuses[rideID], status)
	appUnsentMutex.Unlock()

	chairUnsentMutex.Lock()
	chairUnsentStatuses[rideID] = append(chairUnsentStatuses[rideID], status)
	chairUnsentMutex.Unlock()

	var ride Ride
	if err := db.Get(&ride, `SELECT user_id, chair_id FROM rides WHERE id = ?`, rideID); err != nil {
		return
	}

	notificationMutex.RLock()
	if conn, ok := appNotificationConnections[ride.UserID]; ok {
		go sendAppNotification(conn, rideID, status)
	}
	notificationMutex.RUnlock()

	if ride.ChairID.Valid {
		notificationMutex.RLock()
		if conn, ok := chairNotificationConnections[ride.ChairID.String]; ok {
			go sendChairNotification(conn, rideID, status)
		}
		notificationMutex.RUnlock()
	}
}

// app用: 未送信ステータス取得（SELECTの代わり）
func getAppUnsentStatuses(rideID string) []RideStatus {
	appUnsentMutex.RLock()
	defer appUnsentMutex.RUnlock()
	// コピーを返す（スライス共有を避ける）
	result := make([]RideStatus, len(appUnsentStatuses[rideID]))
	copy(result, appUnsentStatuses[rideID])
	return result
}

// app用: 送信済みマーク後にキャッシュから削除
func markAppStatusSent(rideID, statusID string) {
	appUnsentMutex.Lock()
	defer appUnsentMutex.Unlock()
	statuses := appUnsentStatuses[rideID]
	for i, s := range statuses {
		if s.ID == statusID {
			appUnsentStatuses[rideID] = append(statuses[:i], statuses[i+1:]...)
			break
		}
	}
}

// chair用: 未送信ステータス取得（SELECTの代わり）
func getChairUnsentStatuses(rideID string) []RideStatus {
	chairUnsentMutex.RLock()
	defer chairUnsentMutex.RUnlock()
	// コピーを返す（スライス共有を避ける）
	result := make([]RideStatus, len(chairUnsentStatuses[rideID]))
	copy(result, chairUnsentStatuses[rideID])
	return result
}

// chair用: 送信済みマーク後にキャッシュから削除
func markChairStatusSent(rideID, statusID string) {
	chairUnsentMutex.Lock()
	defer chairUnsentMutex.Unlock()
	statuses := chairUnsentStatuses[rideID]
	for i, s := range statuses {
		if s.ID == statusID {
			chairUnsentStatuses[rideID] = append(statuses[:i], statuses[i+1:]...)
			break
		}
	}
}

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

	notificationMutex.Lock()
	appNotificationConnections = make(map[string]*notificationConnection)
	chairNotificationConnections = make(map[string]*notificationConnection)
	notificationMutex.Unlock()

	// chair_locationsバッファをクリア
	chairLocationBufferMutex.Lock()
	chairLocationBuffer = []ChairLocation{}
	chairLocationBufferMutex.Unlock()

	// 未送信ステータスキャッシュをクリア
	appUnsentMutex.Lock()
	appUnsentStatuses = make(map[string][]RideStatus)
	appUnsentMutex.Unlock()
	chairUnsentMutex.Lock()
	chairUnsentStatuses = make(map[string][]RideStatus)
	chairUnsentMutex.Unlock()

	// Chairキャッシュをクリア
	chairCacheMutex.Lock()
	chairCacheByAccessToken = make(map[string]*Chair)
	chairCacheMutex.Unlock()

	// chairCurrentRideCacheをクリア
	chairCurrentRideCacheMutex.Lock()
	chairCurrentRideCache = make(map[string]string)
	chairCurrentRideCacheMutex.Unlock()

	go func() {
		if _, err := http.Get("http://172.31.14.32:9000/api/group/collect"); err != nil {
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

func sendAppNotification(conn *notificationConnection, rideID string, status RideStatus) {
	select {
	case <-conn.ctx.Done():
		return
	default:
	}

	tx, err := db.Beginx()
	if err != nil {
		return
	}
	defer tx.Rollback()

	ride := &Ride{}
	if err := tx.GetContext(conn.ctx, ride, `SELECT *, latest_status FROM rides WHERE id = ?`, rideID); err != nil {
		return
	}

	userID := ride.UserID

	fare, err := calculateDiscountedFare(conn.ctx, tx, userID, ride, ride.PickupLatitude, ride.PickupLongitude, ride.DestinationLatitude, ride.DestinationLongitude)
	if err != nil {
		return
	}

	responseData := &appGetNotificationResponseData{
		RideID: ride.ID,
		PickupCoordinate: Coordinate{
			Latitude:  ride.PickupLatitude,
			Longitude: ride.PickupLongitude,
		},
		DestinationCoordinate: Coordinate{
			Latitude:  ride.DestinationLatitude,
			Longitude: ride.DestinationLongitude,
		},
		Fare:      fare,
		Status:    status.Status,
		CreatedAt: ride.CreatedAt.UnixMilli(),
		UpdateAt:  ride.UpdatedAt.UnixMilli(),
	}

	if ride.ChairID.Valid {
		chair := &Chair{}
		if err := tx.GetContext(conn.ctx, chair, `SELECT * FROM chairs WHERE id = ?`, ride.ChairID); err != nil {
			return
		}

		stats, err := getChairStats(conn.ctx, tx, chair.ID)
		if err != nil {
			return
		}

		responseData.Chair = &appGetNotificationResponseChair{
			ID:    chair.ID,
			Name:  chair.Name,
			Model: chair.Model,
			Stats: stats,
		}
	}

	data, err := json.Marshal(responseData)
	if err != nil {
		return
	}

	_, err = tx.ExecContext(conn.ctx, `UPDATE ride_statuses SET app_sent_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, status.ID)
	if err != nil {
		return
	}

	if err := tx.Commit(); err != nil {
		return
	}

	markAppStatusSent(rideID, status.ID)

	fmt.Fprintf(conn.w, "data:%s\n\n", data)
	conn.flusher.Flush()
}

func sendChairNotification(conn *notificationConnection, rideID string, status RideStatus) {
	select {
	case <-conn.ctx.Done():
		return
	default:
	}

	tx, err := db.Beginx()
	if err != nil {
		return
	}
	defer tx.Rollback()

	ride := &Ride{}
	if err := tx.GetContext(conn.ctx, ride, `SELECT * FROM rides WHERE id = ?`, rideID); err != nil {
		return
	}

	user := &User{}
	if err := tx.GetContext(conn.ctx, user, "SELECT * FROM users WHERE id = ? FOR SHARE", ride.UserID); err != nil {
		return
	}

	responseData := &chairGetNotificationResponseData{
		RideID: ride.ID,
		User: simpleUser{
			ID:   user.ID,
			Name: fmt.Sprintf("%s %s", user.Firstname, user.Lastname),
		},
		PickupCoordinate: Coordinate{
			Latitude:  ride.PickupLatitude,
			Longitude: ride.PickupLongitude,
		},
		DestinationCoordinate: Coordinate{
			Latitude:  ride.DestinationLatitude,
			Longitude: ride.DestinationLongitude,
		},
		Status: status.Status,
	}

	data, err := json.Marshal(responseData)
	if err != nil {
		return
	}

	_, err = tx.ExecContext(conn.ctx, `UPDATE ride_statuses SET chair_sent_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, status.ID)
	if err != nil {
		return
	}

	sentCompleted := false
	if status.Status == "COMPLETED" {
		if ride.ChairID.Valid {
			if _, err := tx.ExecContext(conn.ctx, `UPDATE chairs SET current_ride_id = NULL WHERE id = ?`, ride.ChairID.String); err != nil {
				return
			}
			sentCompleted = true
		}
	}

	if err := tx.Commit(); err != nil {
		return
	}

	markChairStatusSent(rideID, status.ID)

	if sentCompleted && ride.ChairID.Valid {
		setChairCurrentRideID(ride.ChairID.String, "")
	}

	fmt.Fprintf(conn.w, "data:%s\n\n", data)
	conn.flusher.Flush()
}
