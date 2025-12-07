ALTER TABLE chairs ADD COLUMN current_ride_id VARCHAR(26) NULL COMMENT '現在割り当てられているride_id';
ALTER TABLE chairs ADD INDEX idx_matching (is_active, current_ride_id, latest_latitude, latest_longitude);

-- 既存の未完了ライドの current_ride_id を初期化
UPDATE chairs c
JOIN rides r ON c.id = r.chair_id
SET c.current_ride_id = r.id
WHERE r.latest_status IS NULL OR r.latest_status != 'COMPLETED';
