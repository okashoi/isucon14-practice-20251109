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
		SELECT c.*
		FROM chairs c
		WHERE c.is_active = TRUE
		AND c.latest_latitude IS NOT NULL
		AND c.latest_longitude IS NOT NULL
		AND c.current_ride_id IS NULL
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
	if _, err := db.ExecContext(ctx, "UPDATE chairs SET current_ride_id = ? WHERE id = ?", ride.ID, matched.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// キャッシュを更新
	setChairCurrentRideID(matched.ID, ride.ID)

	w.WriteHeader(http.StatusNoContent)
}
