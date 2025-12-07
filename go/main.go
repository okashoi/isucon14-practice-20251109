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
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/kaz/pprotein/integration"
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


// chairs更新用バッファ
type ChairUpdateData struct {
	LatestLatitude          int
	LatestLongitude         int
	LatestLocationUpdatedAt time.Time
	DistanceToAdd           int
}

var (
	chairUpdateBuffer      = make(map[string]*ChairUpdateData)
	chairUpdateBufferMutex sync.Mutex
	matchingChan = make(chan struct{}, 1000)
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

// 椅子の最新位置キャッシュ (chair_id -> ChairLocation)
var (
	chairLatestLocationCache      = make(map[string]*ChairLocation)
	chairLatestLocationCacheMutex sync.RWMutex
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

// 椅子の最新位置を取得
func getChairLatestLocation(chairID string) *ChairLocation {
	chairLatestLocationCacheMutex.RLock()
	defer chairLatestLocationCacheMutex.RUnlock()
	return chairLatestLocationCache[chairID]
}

// 椅子の最新位置を設定
func setChairLatestLocation(chairID string, location *ChairLocation) {
	chairLatestLocationCacheMutex.Lock()
	defer chairLatestLocationCacheMutex.Unlock()
	chairLatestLocationCache[chairID] = location
}

// 椅子の最新位置キャッシュを初期化（DBから読み込み）
func initChairLatestLocationCache() {
	chairLatestLocationCacheMutex.Lock()
	defer chairLatestLocationCacheMutex.Unlock()

	// キャッシュをクリア
	chairLatestLocationCache = make(map[string]*ChairLocation)

	// chairsテーブルから最新位置を持つ椅子を取得
	type chairLocation struct {
		ID                      string     `db:"id"`
		LatestLatitude          *int       `db:"latest_latitude"`
		LatestLongitude         *int       `db:"latest_longitude"`
		LatestLocationUpdatedAt *time.Time `db:"latest_location_updated_at"`
	}
	chairs := []chairLocation{}
	if err := db.Select(&chairs, "SELECT id, latest_latitude, latest_longitude, latest_location_updated_at FROM chairs WHERE latest_latitude IS NOT NULL"); err != nil {
		slog.Error("failed to load chair locations", "error", err)
		return
	}

	for _, c := range chairs {
		if c.LatestLatitude != nil && c.LatestLongitude != nil {
			loc := &ChairLocation{
				ChairID:   c.ID,
				Latitude:  *c.LatestLatitude,
				Longitude: *c.LatestLongitude,
			}
			if c.LatestLocationUpdatedAt != nil {
				loc.CreatedAt = *c.LatestLocationUpdatedAt
			}
			chairLatestLocationCache[c.ID] = loc
		}
	}
	slog.Info("chair location cache initialized", "count", len(chairLatestLocationCache))
}

// INSERT後にキャッシュに追加
func addUnsentStatus(rideID string, status RideStatus) {
	appUnsentMutex.Lock()
	appUnsentStatuses[rideID] = append(appUnsentStatuses[rideID], status)
	appUnsentMutex.Unlock()

	chairUnsentMutex.Lock()
	chairUnsentStatuses[rideID] = append(chairUnsentStatuses[rideID], status)
	chairUnsentMutex.Unlock()
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

	pproteinHandler := integration.NewDebugHandler()
	go http.ListenAndServe(":3000", pproteinHandler)

	// 椅子の最新位置キャッシュを初期化
	initChairLatestLocationCache()

	// chair_locations のバルクインサート用goroutineを起動
	go bulkInsertChairLocations()


	// chairs テーブルのバルク更新用goroutineを起動
	go bulkUpdateChairs()

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

	// chairs更新バッファをクリア
	chairUpdateBufferMutex.Lock()
	chairUpdateBuffer = make(map[string]*ChairUpdateData)
	chairUpdateBufferMutex.Unlock()

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

	// 椅子の最新位置キャッシュを初期化（DBから読み込み）
	initChairLatestLocationCache()

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

	// 真のバルクインサート: INSERT INTO ... VALUES (...), (...), ...
	valueStrings := make([]string, 0, len(locations))
	valueArgs := make([]interface{}, 0, len(locations)*5)
	for _, loc := range locations {
		valueStrings = append(valueStrings, "(?, ?, ?, ?, ?)")
		valueArgs = append(valueArgs, loc.ID, loc.ChairID, loc.Latitude, loc.Longitude, loc.CreatedAt)
	}
	query := "INSERT INTO chair_locations (id, chair_id, latitude, longitude, created_at) VALUES " + strings.Join(valueStrings, ",")
	_, err := db.Exec(query, valueArgs...)
	if err != nil {
		slog.Error("bulk insert chair_locations failed", "error", err, "count", len(locations))
	}
}

// chairs テーブルのバルク更新処理
func bulkUpdateChairs() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		chairUpdateBufferMutex.Lock()
		if len(chairUpdateBuffer) == 0 {
			chairUpdateBufferMutex.Unlock()
			continue
		}

		// バッファをコピーして即座にクリア
		updates := make(map[string]*ChairUpdateData, len(chairUpdateBuffer))
		for k, v := range chairUpdateBuffer {
			updates[k] = v
		}
		chairUpdateBuffer = make(map[string]*ChairUpdateData)
		chairUpdateBufferMutex.Unlock()

		// バルクアップデート実行
		if len(updates) > 0 {
			updateChairsBulk(updates)
		}
	}
}

func updateChairsBulk(updates map[string]*ChairUpdateData) {
	if len(updates) == 0 {
		return
	}

	// CASE文を使ったバルクUPDATE
	var ids []string
	for id := range updates {
		ids = append(ids, id)
	}

	// クエリ構築
	var latCases, lonCases, updatedAtCases, distCases []string
	args := make([]interface{}, 0, len(updates)*5+len(updates))

	for _, id := range ids {
		data := updates[id]
		latCases = append(latCases, "WHEN ? THEN ?")
		args = append(args, id, data.LatestLatitude)
	}
	for _, id := range ids {
		data := updates[id]
		lonCases = append(lonCases, "WHEN ? THEN ?")
		args = append(args, id, data.LatestLongitude)
	}
	for _, id := range ids {
		data := updates[id]
		updatedAtCases = append(updatedAtCases, "WHEN ? THEN ?")
		args = append(args, id, data.LatestLocationUpdatedAt)
	}
	for _, id := range ids {
		data := updates[id]
		distCases = append(distCases, "WHEN ? THEN total_distance + ?")
		args = append(args, id, data.DistanceToAdd)
	}

	// IN句用のプレースホルダ
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}

	query := fmt.Sprintf(`UPDATE chairs SET 
		latest_latitude = CASE id %s END,
		latest_longitude = CASE id %s END,
		latest_location_updated_at = CASE id %s END,
		total_distance = CASE id %s END
		WHERE id IN (%s)`,
		strings.Join(latCases, " "),
		strings.Join(lonCases, " "),
		strings.Join(updatedAtCases, " "),
		strings.Join(distCases, " "),
		strings.Join(placeholders, ","))

	_, err := db.Exec(query, args...)
	if err != nil {
		slog.Error("bulk update chairs failed", "error", err, "count", len(updates))

func matchingWorker() {
	ctx := context.Background()
	for range matchingChan {
		runMatching(ctx)

	}
}
