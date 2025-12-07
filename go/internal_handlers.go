package main

import (
	"net/http"
	"strings"
)

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// マッチング待ちのライドをすべて取得
	rides := []Ride{}
	if err := db.SelectContext(ctx, &rides, `SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(rides) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 利用可能な椅子をすべて取得
	availableChairs := []Chair{}
	if err := db.SelectContext(ctx, &availableChairs, `
		SELECT *
		FROM chairs
		WHERE is_active = TRUE
		AND latest_latitude IS NOT NULL
		AND latest_longitude IS NOT NULL
		AND current_ride_id IS NULL
	`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(availableChairs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 椅子の利用可能状態を追跡するマップ
	chairUsed := make(map[string]bool)

	// マッチング結果を格納
	type matchResult struct {
		rideID  string
		chairID string
		userID  string
	}
	matches := []matchResult{}

	// 各ライドに対して最も近い椅子をマッチング
	for _, ride := range rides {
		var bestChair *Chair
		bestDistance := int(^uint(0) >> 1) // 最大int値

		for i := range availableChairs {
			chair := availableChairs[i]
			if chairUsed[chair.ID] {
				continue
			}

			// マンハッタン距離を計算
			distance := abs(*chair.LatestLatitude-ride.PickupLatitude) + abs(*chair.LatestLongitude-ride.PickupLongitude)
			if distance < bestDistance {
				bestDistance = distance
				bestChair = &chair
			}
		}

		if bestChair != nil {
			chairUsed[bestChair.ID] = true
			matches = append(matches, matchResult{
				rideID:  ride.ID,
				chairID: bestChair.ID,
				userID:  ride.UserID,
			})
		}
	}

	if len(matches) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// トランザクション開始
	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	// バルク更新: rides テーブル
	// CASE文を使って一括更新
	rideIDs := make([]string, len(matches))
	for i, m := range matches {
		rideIDs[i] = m.rideID
	}

	// rides テーブルの更新
	rideUpdateQuery := "UPDATE rides SET chair_id = CASE id "
	rideUpdateArgs := []interface{}{}
	for _, m := range matches {
		rideUpdateQuery += "WHEN ? THEN ? "
		rideUpdateArgs = append(rideUpdateArgs, m.rideID, m.chairID)
	}
	rideUpdateQuery += "END WHERE id IN (?" + strings.Repeat(", ?", len(rideIDs)-1) + ")"
	for _, id := range rideIDs {
		rideUpdateArgs = append(rideUpdateArgs, id)
	}

	if _, err := tx.ExecContext(ctx, rideUpdateQuery, rideUpdateArgs...); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// chairs テーブルの更新
	chairIDs := make([]string, len(matches))
	for i, m := range matches {
		chairIDs[i] = m.chairID
	}

	chairUpdateQuery := "UPDATE chairs SET current_ride_id = CASE id "
	chairUpdateArgs := []interface{}{}
	for _, m := range matches {
		chairUpdateQuery += "WHEN ? THEN ? "
		chairUpdateArgs = append(chairUpdateArgs, m.chairID, m.rideID)
	}
	chairUpdateQuery += "END WHERE id IN (?" + strings.Repeat(", ?", len(chairIDs)-1) + ")"
	for _, id := range chairIDs {
		chairUpdateArgs = append(chairUpdateArgs, id)
	}

	if _, err := tx.ExecContext(ctx, chairUpdateQuery, chairUpdateArgs...); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// トランザクションコミット
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// マッチング成立を即座に通知
	notificationMutex.RLock()
	for _, m := range matches {
		if ch, ok := appNotificationChannels[m.userID]; ok {
			select {
			case ch <- struct{}{}:
			default: // ブロッキング回避
			}
		}
		if ch, ok := chairNotificationChannels[m.chairID]; ok {
			select {
			case ch <- struct{}{}:
			default: // ブロッキング回避
			}
		}
	}
	notificationMutex.RUnlock()

	w.WriteHeader(http.StatusNoContent)
}
