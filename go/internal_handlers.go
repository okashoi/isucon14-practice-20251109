package main

import (
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
// 最小費用流問題として全体最適なマッチングを計算する
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1回のマッチングで処理する最大ライド数（nearby-chairsとの整合性のため1件ずつ）
	const maxMatchingPerCall = 1

	// 1. 未マッチライドを取得（上限付き）
	rides := []Ride{}
	if err := db.SelectContext(ctx, &rides, `SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at LIMIT ?`, maxMatchingPerCall); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(rides) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 2. 利用可能な椅子を距離順で取得（上位10件に制限して処理を高速化）
	// current_ride_id IS NULL で空き椅子を判定
	ride := rides[0] // 1件のみ取得しているので最初のライドを使用
	chairs := []Chair{}
	chairQuery := `
		SELECT c.*, c.latest_latitude, c.latest_longitude
		FROM chairs c
		WHERE c.is_active = TRUE
		AND c.latest_latitude IS NOT NULL
		AND c.latest_longitude IS NOT NULL
		AND c.current_ride_id IS NULL
		ORDER BY 
			(c.latest_latitude - ?) * (c.latest_latitude - ?) + 
			(c.latest_longitude - ?) * (c.latest_longitude - ?)
		LIMIT 10
	`
	if err := db.SelectContext(ctx, &chairs, chairQuery,
		ride.PickupLatitude, ride.PickupLatitude,
		ride.PickupLongitude, ride.PickupLongitude,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(chairs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 3. 最小費用流グラフを構築
	// ノード: source(0), rides(1..n), chairs(n+1..n+m), sink(n+m+1)
	n := len(rides)
	m := len(chairs)
	source := 0
	sink := n + m + 1
	mcf := NewMinCostFlow(n + m + 2)

	// source → rides (容量1, コスト0)
	for i := 0; i < n; i++ {
		rideNode := i + 1
		mcf.AddEdge(source, rideNode, 1, 0)
	}

	// rides → chairs (容量1, コスト=平方距離を待ち時間で調整)
	now := time.Now()
	for i, ride := range rides {
		rideNode := i + 1
		// 待ち時間（秒）を計算
		waitSeconds := now.Sub(ride.CreatedAt).Seconds()
		// 待ち時間が長いほどコストを下げる係数（30秒で1/4に）
		waitFactor := 1.0 / (1.0 + waitSeconds/10.0)

		for j, chair := range chairs {
			chairNode := n + 1 + j
			// 平方距離をコストとして使用
			latDiff := int64(*chair.LatestLatitude - ride.PickupLatitude)
			lonDiff := int64(*chair.LatestLongitude - ride.PickupLongitude)
			distCost := latDiff*latDiff + lonDiff*lonDiff
			// 待ち時間で調整したコスト
			cost := int64(float64(distCost) * waitFactor)
			mcf.AddEdge(rideNode, chairNode, 1, cost)
		}
	}

	// chairs → sink (容量1, コスト0)
	for j := 0; j < m; j++ {
		chairNode := n + 1 + j
		mcf.AddEdge(chairNode, sink, 1, 0)
	}

	// 4. 最小費用流を計算（最大フロー = min(n, m)）
	maxFlow := n
	if m < n {
		maxFlow = m
	}
	mcf.Run(source, sink, maxFlow)

	// 5. マッチング結果を抽出
	// フローが流れたエッジからマッチングを特定
	type Match struct {
		rideIdx  int
		chairIdx int
	}
	matches := []Match{}

	for i := 0; i < n; i++ {
		rideNode := i + 1
		for _, e := range mcf.graph[rideNode] {
			// 椅子ノードへのエッジで、容量が0になっている（フローが流れた）ものを探す
			if e.to > n && e.to <= n+m && e.cap == 0 {
				chairIdx := e.to - n - 1
				matches = append(matches, Match{rideIdx: i, chairIdx: chairIdx})
				break
			}
		}
	}

	if len(matches) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 6. マッチング結果をDBに反映
	for _, match := range matches {
		ride := rides[match.rideIdx]
		chair := chairs[match.chairIdx]
		if _, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", chair.ID, ride.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if _, err := db.ExecContext(ctx, "UPDATE chairs SET current_ride_id = ? WHERE id = ?", ride.ID, chair.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	// 7. マッチング成立を通知
	notificationMutex.RLock()
	for _, match := range matches {
		ride := rides[match.rideIdx]
		chair := chairs[match.chairIdx]

		// ユーザーへの通知
		if ch, ok := appNotificationChannels[ride.UserID]; ok {
			select {
			case ch <- struct{}{}:
			default: // ブロッキング回避
			}
		}
		// 椅子への通知
		if ch, ok := chairNotificationChannels[chair.ID]; ok {
			select {
			case ch <- struct{}{}:
			default: // ブロッキング回避
			}
		}
	}
	notificationMutex.RUnlock()

	w.WriteHeader(http.StatusNoContent)
}
