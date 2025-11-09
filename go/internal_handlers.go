package main

import (
	"database/sql"
	"errors"
	"math"
	"net/http"
)

// Edge represents an edge in the flow network
type Edge struct {
	to       int
	capacity int
	cost     int
	rev      int // reverse edge index
}

// MinCostFlow represents a minimum cost flow graph
type MinCostFlow struct {
	graph         [][]Edge
	dist          []int
	parent        []int
	parentEdgeIdx []int
	nodeCount     int
}

// NewMinCostFlow creates a new minimum cost flow graph
func NewMinCostFlow(n int) *MinCostFlow {
	return &MinCostFlow{
		graph:         make([][]Edge, n),
		dist:          make([]int, n),
		parent:        make([]int, n),
		parentEdgeIdx: make([]int, n),
		nodeCount:     n,
	}
}

// AddEdge adds a directed edge with capacity and cost
func (mcf *MinCostFlow) AddEdge(from, to, capacity, cost int) {
	mcf.graph[from] = append(mcf.graph[from], Edge{
		to:       to,
		capacity: capacity,
		cost:     cost,
		rev:      len(mcf.graph[to]),
	})
	mcf.graph[to] = append(mcf.graph[to], Edge{
		to:       from,
		capacity: 0,
		cost:     -cost,
		rev:      len(mcf.graph[from]) - 1,
	})
}

// bellmanFord finds shortest path from s to t using Bellman-Ford
func (mcf *MinCostFlow) bellmanFord(s, t int) bool {
	INF := math.MaxInt32
	for i := range mcf.dist {
		mcf.dist[i] = INF
	}
	mcf.dist[s] = 0

	// Bellman-Ford algorithm
	for iter := 0; iter < mcf.nodeCount; iter++ {
		updated := false
		for v := 0; v < mcf.nodeCount; v++ {
			if mcf.dist[v] == INF {
				continue
			}
			for i, e := range mcf.graph[v] {
				if e.capacity > 0 && mcf.dist[v]+e.cost < mcf.dist[e.to] {
					mcf.dist[e.to] = mcf.dist[v] + e.cost
					mcf.parent[e.to] = v
					mcf.parentEdgeIdx[e.to] = i
					updated = true
				}
			}
		}
		if !updated {
			break
		}
	}

	return mcf.dist[t] != INF
}

// Flow calculates minimum cost maximum flow from s to t
func (mcf *MinCostFlow) Flow(s, t, maxFlow int) (flow, cost int) {
	flow = 0
	cost = 0

	for flow < maxFlow {
		// Find shortest path using Bellman-Ford
		if !mcf.bellmanFord(s, t) {
			break
		}

		// Find minimum capacity along the path
		minCap := maxFlow - flow
		for v := t; v != s; v = mcf.parent[v] {
			pv := mcf.parent[v]
			ei := mcf.parentEdgeIdx[v]
			if mcf.graph[pv][ei].capacity < minCap {
				minCap = mcf.graph[pv][ei].capacity
			}
		}

		// Update flow and cost
		flow += minCap
		cost += minCap * mcf.dist[t]

		// Update capacities along the path
		for v := t; v != s; v = mcf.parent[v] {
			pv := mcf.parent[v]
			ei := mcf.parentEdgeIdx[v]

			mcf.graph[pv][ei].capacity -= minCap

			revIdx := mcf.graph[pv][ei].rev
			mcf.graph[v][revIdx].capacity += minCap
		}
	}

	return flow, cost
}

// calculateDistanceSquared calculates squared distance between two points
func calculateDistanceSquared(lat1, lon1, lat2, lon2 int) int {
	dLat := lat1 - lat2
	dLon := lon1 - lon2
	return dLat*dLat + dLon*dLon
}

// ChairWithSpeed represents a chair with its speed information
type ChairWithSpeed struct {
	Chair
	Speed int
}

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	// 全ての未マッチングリクエストを取得
	rides := []Ride{}
	if err := db.SelectContext(ctx, &rides, 
		`SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at`); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if len(rides) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 全ての利用可能な椅子をスピード情報付きで取得
	chairs := []ChairWithSpeed{}
	query := `
		SELECT c.*, cm.speed
		FROM chairs c
		INNER JOIN chair_models cm ON c.model = cm.name
		WHERE c.is_active = TRUE
		AND c.latest_latitude IS NOT NULL
		AND c.latest_longitude IS NOT NULL
		AND NOT EXISTS (
			SELECT 1
			FROM rides r
			WHERE r.chair_id = c.id
			AND EXISTS (
				SELECT 1
				FROM ride_statuses rs
				WHERE rs.ride_id = r.id
				GROUP BY rs.ride_id
				HAVING COUNT(rs.chair_sent_at) < 6
			)
		)
	`
	if err := db.SelectContext(ctx, &chairs, query); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if len(chairs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 最小費用流グラフを構築
	// 参考: https://github.com/ponyo877/isucon14/blob/fa9dbe439b1af705b175b166891d46a801e34719/go/internal_handlers.go#L129
	// ノード番号: 0=source, 1~len(chairs)=chairs, len(chairs)+1~len(chairs)+len(rides)=rides, last=sink
	numChairs := len(chairs)
	numRides := len(rides)
	source := 0
	sink := 1 + numChairs + numRides
	
	mcf := NewMinCostFlow(sink + 1)

	// source -> chairs (容量1, コスト0)
	for i := 0; i < numChairs; i++ {
		chairNode := 1 + i
		mcf.AddEdge(source, chairNode, 1, 0)
	}

	// chairs -> rides (容量1, コスト=到達時間 distance/speed)
	for i, chair := range chairs {
		chairNode := 1 + i
		for j, ride := range rides {
			rideNode := 1 + numChairs + j
			distance := calculateDistance(
				*chair.LatestLatitude, *chair.LatestLongitude,
				ride.PickupLatitude, ride.PickupLongitude,
			)
			// コストは到達時間 (distance / speed)
			cost := distance / chair.Speed
			mcf.AddEdge(chairNode, rideNode, 1, cost)
		}
	}

	// rides -> sink (容量1, コスト0)
	for j := 0; j < numRides; j++ {
		rideNode := 1 + numChairs + j
		mcf.AddEdge(rideNode, sink, 1, 0)
	}

	// 最小費用流を計算
	maxPossibleFlow := numChairs
	if numRides < maxPossibleFlow {
		maxPossibleFlow = numRides
	}
	
	flow, _ := mcf.Flow(source, sink, maxPossibleFlow)

	if flow == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// マッチング結果を抽出
	matchings := []struct {
		rideID  string
		chairID string
		userID  string
	}{}

	// 各chairノードからrideノードへの辺を確認
	for i := 0; i < numChairs; i++ {
		chairNode := 1 + i
		for _, edge := range mcf.graph[chairNode] {
			// rideノードへの辺で、容量が0（=流れた）ものを探す
			if edge.to > numChairs && edge.to <= numChairs+numRides && edge.capacity == 0 {
				rideIdx := edge.to - 1 - numChairs
				// 逆向きエッジの容量が1なら確実に流れている
				revEdge := mcf.graph[edge.to][edge.rev]
				if revEdge.capacity == 1 {
					matchings = append(matchings, struct {
						rideID  string
						chairID string
						userID  string
					}{
						rideID:  rides[rideIdx].ID,
						chairID: chairs[i].ID,
						userID:  rides[rideIdx].UserID,
					})
					break
				}
			}
		}
	}

	if len(matchings) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// データベースを更新
	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	for _, m := range matchings {
		if _, err := tx.ExecContext(ctx,
			"UPDATE rides SET chair_id = ? WHERE id = ?",
			m.chairID, m.rideID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// マッチング成立を即座に通知
	notificationMutex.RLock()
	for _, m := range matchings {
		if ch, ok := appNotificationChannels[m.userID]; ok {
			select {
			case ch <- struct{}{}:
			default: // ブロッキング回避
			}
		}
		if ch, ok := chairNotificationChannels[m.chairID]; ok {
			select {
			case ch <- struct{}{}:
			default: // ブロッキング回避
			}
		}
	}
	notificationMutex.RUnlock()

	w.WriteHeader(http.StatusNoContent)
}
