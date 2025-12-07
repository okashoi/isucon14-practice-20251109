package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
)

func runMatching(ctx context.Context, rideID string) {
	tx, err := db.Beginx()
	if err != nil {
		slog.Error("failed to begin transaction", "error", err)
		return
	}
	defer tx.Rollback()

	ride := &Ride{}
	if err := tx.GetContext(ctx, ride, `SELECT * FROM rides WHERE id = ? AND chair_id IS NULL`, rideID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		slog.Error("failed to get ride for matching", "error", err, "ride_id", rideID)
		return
	}

	// pickup_latitudeに応じて適切なchairを検索
	var matched *Chair
	if ride.PickupLatitude <= 150 {
		matched = &Chair{}
		query := `
			SELECT c.*
			FROM chairs c
			WHERE c.is_active = TRUE
			AND c.latest_latitude IS NOT NULL
			AND c.latest_latitude <= 150
			AND c.latest_longitude IS NOT NULL
			AND c.current_ride_id IS NULL
			ORDER BY 
				(c.latest_latitude - ?) * (c.latest_latitude - ?) + 
				(c.latest_longitude - ?) * (c.latest_longitude - ?)
			LIMIT 1
		`
		if err := tx.GetContext(ctx, matched, query,
			ride.PickupLatitude, ride.PickupLatitude,
			ride.PickupLongitude, ride.PickupLongitude,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return
			}
			slog.Error("failed to get matched chair", "error", err)
			return
		}
	} else {
		matched = &Chair{}
		query := `
			SELECT c.*
			FROM chairs c
			WHERE c.is_active = TRUE
			AND c.latest_latitude IS NOT NULL
			AND c.latest_latitude > 150
			AND c.latest_longitude IS NOT NULL
			AND c.current_ride_id IS NULL
			ORDER BY 
				(c.latest_latitude - ?) * (c.latest_latitude - ?) + 
				(c.latest_longitude - ?) * (c.latest_longitude - ?)
			LIMIT 1
		`
		if err := tx.GetContext(ctx, matched, query,
			ride.PickupLatitude, ride.PickupLatitude,
			ride.PickupLongitude, ride.PickupLongitude,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return
			}
			slog.Error("failed to get matched chair", "error", err)
			return
		}
	}

	if _, err := tx.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", matched.ID, ride.ID); err != nil {
		slog.Error("failed to update ride with chair_id", "error", err)
		return
	}
	if _, err := tx.ExecContext(ctx, "UPDATE chairs SET current_ride_id = ? WHERE id = ?", ride.ID, matched.ID); err != nil {
		slog.Error("failed to update chair with current_ride_id", "error", err)
		return
	}

	if err := tx.Commit(); err != nil {
		slog.Error("failed to commit transaction", "error", err)
		return
	}

	setChairCurrentRideID(matched.ID, ride.ID)

	notificationMutex.RLock()
	if ch, ok := appNotificationChannels[ride.UserID]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	if ch, ok := chairNotificationChannels[matched.ID]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	notificationMutex.RUnlock()
}
