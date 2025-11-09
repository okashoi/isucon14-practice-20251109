package main

import (
	"database/sql"
	"errors"
	"net/http"
)

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ride := &Ride{}
	if err := db.GetContext(ctx, ride, `SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at LIMIT 1`); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// pickup座標に近い空いている椅子を取得してアサイン
	matched := &Chair{}
	query := `
		WITH available_chairs AS (
			-- 空いている椅子（is_activeでかつ未完了のrideがない）
			SELECT c.id 
			FROM chairs c
			WHERE c.is_active = TRUE
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
		),
		latest_locations AS (
			-- 各椅子の最新位置
			SELECT 
				chair_id,
				latitude,
				longitude,
				ROW_NUMBER() OVER (PARTITION BY chair_id ORDER BY created_at DESC) AS rn
			FROM chair_locations
			WHERE chair_id IN (SELECT id FROM available_chairs)
		)
		SELECT c.*
		FROM chairs c
		INNER JOIN latest_locations cl ON c.id = cl.chair_id AND cl.rn = 1
		ORDER BY 
			(cl.latitude - ?) * (cl.latitude - ?) + 
			(cl.longitude - ?) * (cl.longitude - ?)
		LIMIT 1
	`
	if err := db.GetContext(ctx, matched, query,
		ride.PickupLatitude, ride.PickupLatitude,
		ride.PickupLongitude, ride.PickupLongitude,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 空いている椅子が見つかったのでアサイン
	if _, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", matched.ID, ride.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
