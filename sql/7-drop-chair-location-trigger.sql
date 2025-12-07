-- chair_locationsのINSERTトリガーを廃止
-- アプリケーション側でchairsテーブルの更新を行うため、トリガーは不要
DROP TRIGGER IF EXISTS trg_chair_locations_after_insert;

