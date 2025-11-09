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

	// pickup座標に近い椅子を最大10件取得
	nearbyChairs := []Chair{}
	query := `
		SELECT c.*
		FROM chairs c
		INNER JOIN (
			SELECT 
				chair_id,
				latitude,
				longitude,
				ROW_NUMBER() OVER (PARTITION BY chair_id ORDER BY created_at DESC) AS rn
			FROM chair_locations
			WHERE chair_id IN (
				SELECT id FROM chairs WHERE is_active = TRUE
			)
		) cl ON c.id = cl.chair_id AND cl.rn = 1
		ORDER BY 
			(cl.latitude - ?) * (cl.latitude - ?) + 
			(cl.longitude - ?) * (cl.longitude - ?)
		LIMIT 10
	`
	if err := db.SelectContext(ctx, &nearbyChairs, query,
		ride.PickupLatitude, ride.PickupLatitude,
		ride.PickupLongitude, ride.PickupLongitude,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 近い順に椅子が空いているかチェックしてアサイン
	for _, chair := range nearbyChairs {
		empty := false
		if err := db.GetContext(ctx, &empty, "SELECT COUNT(*) = 0 FROM (SELECT COUNT(chair_sent_at) = 6 AS completed FROM ride_statuses WHERE ride_id IN (SELECT id FROM rides WHERE chair_id = ?) GROUP BY ride_id) is_completed WHERE completed = FALSE", chair.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if empty {
			// 空いている椅子が見つかったのでアサイン
			if _, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", chair.ID, ride.ID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// 空いている椅子が見つからなかった
	w.WriteHeader(http.StatusNoContent)
}
