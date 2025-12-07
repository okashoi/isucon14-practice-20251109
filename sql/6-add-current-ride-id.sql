ALTER TABLE chairs ADD COLUMN current_ride_id VARCHAR(26) NULL COMMENT '現在割り当てられているride_id';
ALTER TABLE chairs ADD INDEX idx_matching (is_active, current_ride_id, latest_latitude, latest_longitude);
