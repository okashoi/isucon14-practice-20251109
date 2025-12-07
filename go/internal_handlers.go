package main

import (
	"database/sql"
	"errors"
	"math"
	"net/http"
	"time"
)

// 最小費用流アルゴリズム用のエッジ構造体
type Edge struct {
	to   int   // 行き先ノード
	cap  int   // 容量
	cost int64 // コスト
	rev  int   // 逆辺のインデックス
}

// 最小費用流を計算するための構造体
type MinCostFlow struct {
	graph [][]Edge // 隣接リスト表現のグラフ
	n     int      // ノード数
}

// NewMinCostFlow は指定したノード数で MinCostFlow を初期化する
func NewMinCostFlow(n int) *MinCostFlow {
	graph := make([][]Edge, n)
	for i := range graph {
		graph[i] = make([]Edge, 0)
	}
	return &MinCostFlow{graph: graph, n: n}
}

// AddEdge はグラフにエッジを追加する（逆辺も同時に追加）
func (mcf *MinCostFlow) AddEdge(from, to, cap int, cost int64) {
	mcf.graph[from] = append(mcf.graph[from], Edge{to: to, cap: cap, cost: cost, rev: len(mcf.graph[to])})
	mcf.graph[to] = append(mcf.graph[to], Edge{to: from, cap: 0, cost: -cost, rev: len(mcf.graph[from]) - 1})
}

// bellmanFord は source から各ノードへの最短距離を計算する
// 戻り値: (距離配列, 直前ノード配列, 直前エッジ配列, 到達可能かどうか)
func (mcf *MinCostFlow) bellmanFord(source, sink int) ([]int64, []int, []int, bool) {
	const INF = math.MaxInt64

	dist := make([]int64, mcf.n)
	prevNode := make([]int, mcf.n)
	prevEdge := make([]int, mcf.n)

	for i := range dist {
		dist[i] = INF
		prevNode[i] = -1
		prevEdge[i] = -1
	}
	dist[source] = 0

	// Bellman-Ford法: 最大 n-1 回の緩和
	for i := 0; i < mcf.n; i++ {
		updated := false
		for v := 0; v < mcf.n; v++ {
			if dist[v] == INF {
				continue
			}
			for ei, e := range mcf.graph[v] {
				if e.cap > 0 && dist[v]+e.cost < dist[e.to] {
					dist[e.to] = dist[v] + e.cost
					prevNode[e.to] = v
					prevEdge[e.to] = ei
					updated = true
				}
			}
		}
		if !updated {
			break
		}
	}

	return dist, prevNode, prevEdge, dist[sink] != INF
}

// MinCostFlowResult はマッチング結果を表す
type MinCostFlowResult struct {
	flow int
	cost int64
}

// Run は source から sink への最小費用最大流を計算する
// 戻り値: (流量, 総コスト)
func (mcf *MinCostFlow) Run(source, sink, maxFlow int) MinCostFlowResult {
	totalFlow := 0
	totalCost := int64(0)

	for totalFlow < maxFlow {
		// 最短経路を探索
		dist, prevNode, prevEdge, reachable := mcf.bellmanFord(source, sink)
		if !reachable {
			break
		}

		// 経路上の最小容量を計算
		minCap := maxFlow - totalFlow
		for v := sink; v != source; v = prevNode[v] {
			e := mcf.graph[prevNode[v]][prevEdge[v]]
			if e.cap < minCap {
				minCap = e.cap
			}
		}

		// フローを流す
		for v := sink; v != source; v = prevNode[v] {
			e := &mcf.graph[prevNode[v]][prevEdge[v]]
			e.cap -= minCap
			mcf.graph[v][e.rev].cap += minCap
		}

		totalFlow += minCap
		totalCost += dist[sink] * int64(minCap)
	}

	return MinCostFlowResult{flow: totalFlow, cost: totalCost}
}

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

	// 待ち時間を計算
	waitSeconds := time.Since(ride.CreatedAt).Seconds()

	matched := &Chair{}
	var err error

	if waitSeconds >= 25 {
		// 25秒以上待っている場合は、最も近い椅子を優先（スピード無視で緊急マッチング）
		query := `
			SELECT c.*
			FROM chairs c
			WHERE c.is_active = TRUE
			AND c.latest_latitude IS NOT NULL
			AND c.latest_longitude IS NOT NULL
			AND c.current_ride_id IS NULL
			ORDER BY 
				ABS(c.latest_latitude - ?) + ABS(c.latest_longitude - ?)
			LIMIT 1
		`
		err = db.GetContext(ctx, matched, query,
			ride.PickupLatitude, ride.PickupLongitude,
		)
	} else {
		// 通常: 最も早く到着できる椅子を取得（到着時間 = 距離 / スピード）
		query := `
			SELECT c.*
			FROM chairs c
			INNER JOIN chair_models cm ON c.model = cm.name
			WHERE c.is_active = TRUE
			AND c.latest_latitude IS NOT NULL
			AND c.latest_longitude IS NOT NULL
			AND c.current_ride_id IS NULL
			ORDER BY 
				(ABS(c.latest_latitude - ?) + ABS(c.latest_longitude - ?)) / cm.speed
			LIMIT 1
		`
		err = db.GetContext(ctx, matched, query,
			ride.PickupLatitude, ride.PickupLongitude,
		)
	}

	if err != nil {
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

	// マッチング成立を即座に通知
	notificationMutex.RLock()
	if ch, ok := appNotificationChannels[ride.UserID]; ok {
		select {
		case ch <- struct{}{}:
		default: // ブロッキング回避
		}
	}
	if ch, ok := chairNotificationChannels[matched.ID]; ok {
		select {
		case ch <- struct{}{}:
		default: // ブロッキング回避
		}
	}
	notificationMutex.RUnlock()

	w.WriteHeader(http.StatusNoContent)
}
