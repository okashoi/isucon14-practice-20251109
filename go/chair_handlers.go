package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/oklog/ulid/v2"
)

type chairPostChairsRequest struct {
	Name               string `json:"name"`
	Model              string `json:"model"`
	ChairRegisterToken string `json:"chair_register_token"`
}

type chairPostChairsResponse struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
}

func chairPostChairs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := &chairPostChairsRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Model == "" || req.ChairRegisterToken == "" {
		writeError(w, http.StatusBadRequest, errors.New("some of required fields(name, model, chair_register_token) are empty"))
		return
	}

	owner := &Owner{}
	if err := db.GetContext(ctx, owner, "SELECT * FROM owners WHERE chair_register_token = ?", req.ChairRegisterToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, errors.New("invalid chair_register_token"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	chairID := ulid.Make().String()
	accessToken := secureRandomStr(32)

	now := time.Now()
	_, err := db.ExecContext(
		ctx,
		"INSERT INTO chairs (id, owner_id, name, model, is_active, access_token) VALUES (?, ?, ?, ?, ?, ?)",
		chairID, owner.ID, req.Name, req.Model, false, accessToken,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 新規登録したChairをキャッシュに追加
	setChairCache(accessToken, &Chair{
		ID:          chairID,
		OwnerID:     owner.ID,
		Name:        req.Name,
		Model:       req.Model,
		IsActive:    false,
		AccessToken: accessToken,
		CreatedAt:   now,
		UpdatedAt:   now,
	})

	http.SetCookie(w, &http.Cookie{
		Path:  "/",
		Name:  "chair_session",
		Value: accessToken,
	})

	writeJSON(w, http.StatusCreated, &chairPostChairsResponse{
		ID:      chairID,
		OwnerID: owner.ID,
	})
}

type postChairActivityRequest struct {
	IsActive bool `json:"is_active"`
}

func chairPostActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chair := ctx.Value("chair").(*Chair)

	req := &postChairActivityRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	_, err := db.ExecContext(ctx, "UPDATE chairs SET is_active = ? WHERE id = ?", req.IsActive, chair.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type chairPostCoordinateResponse struct {
	RecordedAt int64 `json:"recorded_at"`
}

func chairPostCoordinate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := &Coordinate{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	chair := ctx.Value("chair").(*Chair)

	// chair_locationをバッファに追加
	chairLocationID := ulid.Make().String()
	now := time.Now()
	location := ChairLocation{
		ID:        chairLocationID,
		ChairID:   chair.ID,
		Latitude:  req.Latitude,
		Longitude: req.Longitude,
		CreatedAt: now,
	}

	chairLocationBufferMutex.Lock()
	chairLocationBuffer = append(chairLocationBuffer, location)
	chairLocationBufferMutex.Unlock()

	// ride_statusesの更新のみトランザクション処理
	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	ride := &Ride{}
	var insertedStatusID string
	var insertedStatusName string
	var insertedRideID string
	insertedStatusCreatedAt := time.Now()

	// キャッシュからcurrent_ride_idを取得
	currentRideID, cacheHit := getChairCurrentRideID(chair.ID)
	if !cacheHit {
		// キャッシュミス時はDBから取得してキャッシュを更新
		if chair.CurrentRideID.Valid {
			currentRideID = chair.CurrentRideID.String
		}
		setChairCurrentRideID(chair.ID, currentRideID)
	}

	// current_ride_idがある場合のみrideを取得
	if currentRideID != "" {
		if err := tx.GetContext(ctx, ride, `SELECT *, latest_status FROM rides WHERE id = ?`, currentRideID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		} else {
			status := ""
			if ride.LatestStatus.Valid {
				status = ride.LatestStatus.String
			}
			if status != "COMPLETED" && status != "CANCELED" {
				if req.Latitude == ride.PickupLatitude && req.Longitude == ride.PickupLongitude && status == "ENROUTE" {
					insertedStatusID = ulid.Make().String()
					insertedStatusName = "PICKUP"
					insertedRideID = ride.ID
					if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", insertedStatusID, ride.ID, "PICKUP"); err != nil {
						writeError(w, http.StatusInternalServerError, err)
						return
					}
				}

				if req.Latitude == ride.DestinationLatitude && req.Longitude == ride.DestinationLongitude && status == "CARRYING" {
					insertedStatusID = ulid.Make().String()
					insertedStatusName = "ARRIVED"
					insertedRideID = ride.ID
					if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", insertedStatusID, ride.ID, "ARRIVED"); err != nil {
						writeError(w, http.StatusInternalServerError, err)
						return
					}
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// コミット成功後にキャッシュに追加
	if insertedStatusID != "" {
		addUnsentStatus(insertedRideID, RideStatus{
			ID:        insertedStatusID,
			RideID:    insertedRideID,
			Status:    insertedStatusName,
			CreatedAt: insertedStatusCreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, &chairPostCoordinateResponse{
		RecordedAt: now.UnixMilli(),
	})
}

type simpleUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type chairGetNotificationResponse struct {
	Data         *chairGetNotificationResponseData `json:"data"`
	RetryAfterMs int                               `json:"retry_after_ms"`
}

type chairGetNotificationResponseData struct {
	RideID                string     `json:"ride_id"`
	User                  simpleUser `json:"user"`
	PickupCoordinate      Coordinate `json:"pickup_coordinate"`
	DestinationCoordinate Coordinate `json:"destination_coordinate"`
	Status                string     `json:"status"`
}

func chairGetNotification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chair := ctx.Value("chair").(*Chair)

	// SSEのヘッダー設定
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}

	// チャネル登録
	notifyChan := make(chan struct{}, 10)
	notificationMutex.Lock()
	chairNotificationChannels[chair.ID] = notifyChan
	notificationMutex.Unlock()

	defer func() {
		notificationMutex.Lock()
		delete(chairNotificationChannels, chair.ID)
		notificationMutex.Unlock()
		close(notifyChan)
	}()

	// 未送信の状態遷移を全て送信する関数
	sendNotifications := func() {
		// キャッシュからcurrent_ride_idを取得
		currentRideID, cacheHit := getChairCurrentRideID(chair.ID)
		if !cacheHit {
			// キャッシュミス時はDBから取得してキャッシュを更新
			if chair.CurrentRideID.Valid {
				currentRideID = chair.CurrentRideID.String
			}
			setChairCurrentRideID(chair.ID, currentRideID)
		}

		// current_ride_idがない場合はスキップ
		if currentRideID == "" {
			return
		}

		// 未送信の状態をキャッシュから取得
		yetSentRideStatuses := getChairUnsentStatuses(currentRideID)

		// 未送信の状態がない場合はスキップ（DBアクセス前に判定）
		if len(yetSentRideStatuses) == 0 {
			return
		}

		tx, err := db.Beginx()
		if err != nil {
			return
		}

		ride := &Ride{}
		if err := tx.GetContext(ctx, ride, `SELECT * FROM rides WHERE id = ?`, currentRideID); err != nil {
			tx.Rollback()
			if errors.Is(err, sql.ErrNoRows) {
				return
			}
			return
		}

		user := &User{}
		err = tx.GetContext(ctx, user, "SELECT * FROM users WHERE id = ? FOR SHARE", ride.UserID)
		if err != nil {
			tx.Rollback()
			return
		}

		// 送信済みステータスIDを追跡
		sentStatusIDs := make([]string, 0, len(yetSentRideStatuses))
		sentCompleted := false

		// 各未送信状態を順次送信
		for _, rideStatus := range yetSentRideStatuses {
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
				Status: rideStatus.Status,
			}

			// SSE形式で送信
			data, err := json.Marshal(responseData)
			if err != nil {
				tx.Rollback()
				return
			}
			fmt.Fprintf(w, "data:%s\n\n", data)
			flusher.Flush()

			// 送信済みマーク
			_, err = tx.ExecContext(ctx, `UPDATE ride_statuses SET chair_sent_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, rideStatus.ID)
			if err != nil {
				tx.Rollback()
				return
			}
			sentStatusIDs = append(sentStatusIDs, rideStatus.ID)

			// COMPLETEDステータスを椅子に通知した場合、current_ride_idをクリア
			if rideStatus.Status == "COMPLETED" {
				if _, err := tx.ExecContext(ctx, `UPDATE chairs SET current_ride_id = NULL WHERE id = ?`, chair.ID); err != nil {
					tx.Rollback()
					return
				}
				sentCompleted = true
			}
		}

		if err := tx.Commit(); err != nil {
			return
		}

		// コミット成功後にキャッシュから削除
		for _, statusID := range sentStatusIDs {
			markChairStatusSent(ride.ID, statusID)
		}

		// COMPLETEDが送信された場合、current_ride_idキャッシュをクリア
		if sentCompleted {
			setChairCurrentRideID(chair.ID, "")
		}
	}

	// フォールバック用のticker (500ms間隔)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-notifyChan:
			// マッチング成立時に即座に通知
			sendNotifications()
		case <-ticker.C:
			// フォールバック: 定期的にもチェック
			sendNotifications()
		}
	}
}

type postChairRidesRideIDStatusRequest struct {
	Status string `json:"status"`
}

func chairPostRideStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rideID := r.PathValue("ride_id")

	chair := ctx.Value("chair").(*Chair)

	req := &postChairRidesRideIDStatusRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	ride := &Ride{}
	if err := tx.GetContext(ctx, ride, "SELECT *, latest_status FROM rides WHERE id = ? FOR UPDATE", rideID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("ride not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if ride.ChairID.String != chair.ID {
		writeError(w, http.StatusBadRequest, errors.New("not assigned to this ride"))
		return
	}

	var statusID string
	var statusName string
	statusCreatedAt := time.Now()

	switch req.Status {
	// Acknowledge the ride
	case "ENROUTE":
		statusID = ulid.Make().String()
		statusName = "ENROUTE"
		if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", statusID, ride.ID, "ENROUTE"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	// After Picking up user
	case "CARRYING":
		status := ""
		if ride.LatestStatus.Valid {
			status = ride.LatestStatus.String
		}
		if status != "PICKUP" {
			writeError(w, http.StatusBadRequest, errors.New("chair has not arrived yet"))
			return
		}
		statusID = ulid.Make().String()
		statusName = "CARRYING"
		if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", statusID, ride.ID, "CARRYING"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, errors.New("invalid status"))
		return
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// コミット成功後にキャッシュに追加
	addUnsentStatus(ride.ID, RideStatus{
		ID:        statusID,
		RideID:    ride.ID,
		Status:    statusName,
		CreatedAt: statusCreatedAt,
	})

	w.WriteHeader(http.StatusNoContent)
}
