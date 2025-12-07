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

// 椅子とスピード情報を一緒に保持する構造体
type ChairWithSpeed struct {
	Chair
	Speed int `db:"speed"`
}

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 待機中の全ライドを取得
	var rides []Ride
	if err := db.SelectContext(ctx, &rides, `SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(rides) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 利用可能な全椅子をスピード情報と共に取得
	var chairs []ChairWithSpeed
	query := `
		SELECT c.*, cm.speed
		FROM chairs c
		INNER JOIN chair_models cm ON c.model = cm.name
		WHERE c.is_active = TRUE
		AND c.latest_latitude IS NOT NULL
		AND c.latest_longitude IS NOT NULL
		AND c.current_ride_id IS NULL
	`
	if err := db.SelectContext(ctx, &chairs, query); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(chairs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 最小費用流グラフを構築
	// ノード: source(0), 椅子(1〜M), ライド(M+1〜M+N), sink(M+N+1)
	numChairs := len(chairs)
	numRides := len(rides)
	numNodes := 2 + numChairs + numRides
	source := 0
	sink := numNodes - 1

	mcf := NewMinCostFlow(numNodes)

	// source → 各椅子 (容量1, コスト0)
	for i := 0; i < numChairs; i++ {
		chairNode := 1 + i
		mcf.AddEdge(source, chairNode, 1, 0)
	}

	// 各椅子 → 各ライド (容量1, コスト = 到着時間 - 待ち時間ボーナス)
	now := time.Now()
	for i, chair := range chairs {
		chairNode := 1 + i
		chairLat := *chair.LatestLatitude
		chairLon := *chair.LatestLongitude

		for j, ride := range rides {
			rideNode := 1 + numChairs + j

			// マンハッタン距離を計算
			distance := abs(chairLat-ride.PickupLatitude) + abs(chairLon-ride.PickupLongitude)

			// 到着時間（距離/スピード）を1000倍してint64に変換
			arrivalTime := int64(distance) * 1000 / int64(chair.Speed)

			// 待ち時間ボーナス（待ち時間が長いほどコストを下げる）
			waitSeconds := now.Sub(ride.CreatedAt).Seconds()
			waitBonus := int64(waitSeconds * 100)

			// コスト = 到着時間 - 待ち時間ボーナス
			cost := arrivalTime - waitBonus

			mcf.AddEdge(chairNode, rideNode, 1, cost)
		}
	}

	// 各ライド → sink (容量1, コスト0)
	for j := 0; j < numRides; j++ {
		rideNode := 1 + numChairs + j
		mcf.AddEdge(rideNode, sink, 1, 0)
	}

	// 最小費用流を実行
	maxFlow := numChairs
	if numRides < maxFlow {
		maxFlow = numRides
	}
	mcf.Run(source, sink, maxFlow)

	// マッチング結果を抽出（容量が0になった椅子→ライドのエッジを見つける）
	type Match struct {
		ChairID string
		RideID  string
		UserID  string
	}
	var matches []Match

	for i := 0; i < numChairs; i++ {
		chairNode := 1 + i
		for _, edge := range mcf.graph[chairNode] {
			// ライドノードへのエッジで、容量が0（使用済み）のものを探す
			if edge.to >= 1+numChairs && edge.to < sink && edge.cap == 0 {
				rideIdx := edge.to - 1 - numChairs
				matches = append(matches, Match{
					ChairID: chairs[i].ID,
					RideID:  rides[rideIdx].ID,
					UserID:  rides[rideIdx].UserID,
				})
				break
			}
		}
	}

	if len(matches) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// マッチング結果をDBに反映
	for _, m := range matches {
		if _, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", m.ChairID, m.RideID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if _, err := db.ExecContext(ctx, "UPDATE chairs SET current_ride_id = ? WHERE id = ?", m.RideID, m.ChairID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		// キャッシュを更新
		setChairCurrentRideID(m.ChairID, m.RideID)
	}

	// マッチング成立を通知
	notificationMutex.RLock()
	for _, m := range matches {
		if ch, ok := appNotificationChannels[m.UserID]; ok {
			select {
			case ch <- struct{}{}:
			default: // ブロッキング回避
			}
		}
		if ch, ok := chairNotificationChannels[m.ChairID]; ok {
			select {
			case ch <- struct{}{}:
			default: // ブロッキング回避
			}
		}
	}
	notificationMutex.RUnlock()

	w.WriteHeader(http.StatusNoContent)
}
