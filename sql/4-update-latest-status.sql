-- 既存データのlatest_statusを初期化
-- 各ride_idについて、最新のride_statusesのstatusを取得してrides.latest_statusを更新
UPDATE rides r
INNER JOIN (
  SELECT ride_id, status, created_at
  FROM ride_statuses rs1
  WHERE created_at = (
    SELECT MAX(created_at)
    FROM ride_statuses rs2
    WHERE rs2.ride_id = rs1.ride_id
  )
) latest ON r.id = latest.ride_id
SET r.latest_status = latest.status,
    r.updated_at = latest.created_at;

