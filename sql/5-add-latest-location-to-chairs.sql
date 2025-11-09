-- 既存データの最新位置を移行
UPDATE chairs c
INNER JOIN (
  SELECT chair_id, latitude, longitude, created_at
  FROM chair_locations cl1
  WHERE created_at = (
    SELECT MAX(created_at)
    FROM chair_locations cl2
    WHERE cl2.chair_id = cl1.chair_id
  )
) latest ON c.id = latest.chair_id
SET c.latest_latitude = latest.latitude,
    c.latest_longitude = latest.longitude,
    c.latest_location_updated_at = latest.created_at;

-- chair_locationsへのINSERT時にchairsを更新するトリガー
DROP TRIGGER IF EXISTS trg_chair_locations_after_insert;
DELIMITER //
CREATE TRIGGER trg_chair_locations_after_insert
AFTER INSERT ON chair_locations
FOR EACH ROW
BEGIN
  UPDATE chairs
    SET latest_latitude = NEW.latitude,
        latest_longitude = NEW.longitude,
        latest_location_updated_at = NEW.created_at
  WHERE id = NEW.chair_id;
END//
DELIMITER ;

