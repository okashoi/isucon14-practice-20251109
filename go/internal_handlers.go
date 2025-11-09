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

	// pickup座標に近い椅子を最大5件取得
	nearbyChairs := []Chair{}
	query := `
		SELECT c.* 
		FROM chairs c
		INNER JOIN (
			SELECT cl1.chair_id, cl1.latitude, cl1.longitude
			FROM chair_locations cl1
			INNER JOIN (
				SELECT cl.chair_id, MAX(cl.created_at) AS max_created_at
				FROM chair_locations cl
				INNER JOIN chairs c ON cl.chair_id = c.id
				WHERE c.is_active = TRUE
				GROUP BY cl.chair_id
			) cl2 ON cl1.chair_id = cl2.chair_id AND cl1.created_at = cl2.max_created_at
		) cl ON c.id = cl.chair_id
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
