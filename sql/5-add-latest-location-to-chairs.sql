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

-- 既存データの総移動距離を計算
UPDATE chairs c
SET c.total_distance = COALESCE((
  SELECT SUM(ABS(cl2.latitude - cl1.latitude) + ABS(cl2.longitude - cl1.longitude))
  FROM chair_locations cl1
  INNER JOIN chair_locations cl2 ON cl1.chair_id = cl2.chair_id
  WHERE cl1.chair_id = c.id
    AND cl2.created_at = (
      SELECT MIN(cl3.created_at)
      FROM chair_locations cl3
      WHERE cl3.chair_id = cl1.chair_id
        AND cl3.created_at > cl1.created_at
    )
), 0),
c.total_distance_updated_at = (
  SELECT MAX(created_at)
  FROM chair_locations
  WHERE chair_id = c.id
);

-- chair_locationsへのINSERT時にchairsを更新するトリガー
DROP TRIGGER IF EXISTS trg_chair_locations_after_insert;
DELIMITER //
CREATE TRIGGER trg_chair_locations_after_insert
AFTER INSERT ON chair_locations
FOR EACH ROW
BEGIN
  DECLARE prev_lat INT DEFAULT NULL;
  DECLARE prev_lon INT DEFAULT NULL;
  DECLARE distance_delta INT DEFAULT 0;
  
  -- 前回の位置を取得
  SELECT latitude, longitude INTO prev_lat, prev_lon
  FROM chair_locations
  WHERE chair_id = NEW.chair_id AND created_at < NEW.created_at
  ORDER BY created_at DESC
  LIMIT 1;
  
  -- 距離を計算
  IF prev_lat IS NOT NULL THEN
    SET distance_delta = ABS(NEW.latitude - prev_lat) + ABS(NEW.longitude - prev_lon);
  END IF;
  
  -- 最新位置と総距離を更新
  UPDATE chairs
    SET latest_latitude = NEW.latitude,
        latest_longitude = NEW.longitude,
        latest_location_updated_at = NEW.created_at,
        total_distance = total_distance + distance_delta,
        total_distance_updated_at = NEW.created_at
  WHERE id = NEW.chair_id;
END//
DELIMITER ;

