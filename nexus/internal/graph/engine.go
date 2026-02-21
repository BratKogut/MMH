package graph

import (
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Entity Graph Engine — wallet clustering, label propagation, Sybil detection
// Plan v3.1 Layer 1.5: In-memory graph ~500k nodes, BFS <2ms
// ---------------------------------------------------------------------------

// Label represents a classification applied to a wallet node.
type Label string

const (
	LabelUnknown           Label = "UNKNOWN"
	LabelSerialRugger      Label = "SERIAL_RUGGER"
	LabelProfitableInsider Label = "PROFITABLE_INSIDER"
	LabelWashTrader        Label = "WASH_TRADER"
	LabelSybilCluster      Label = "SYBIL_CLUSTER"
	LabelClean             Label = "CLEAN"
	LabelCEX               Label = "CEX"
)

// GraphNode represents a wallet address in the graph.
type GraphNode struct {
	Address   string    `json:"address"`
	Labels    []Label   `json:"labels"`
	FirstSeen int64     `json:"first_seen"`
	TxCount   uint32    `json:"tx_count"`
	ClusterID uint32    `json:"cluster_id"`
	IsCEX     bool      `json:"is_cex"`
	DeployCount int     `json:"deploy_count"` // tokens deployed by this address
	RugCount    int     `json:"rug_count"`    // rugs attributed to this address
}

// GraphEdge represents a transfer/interaction between wallets.
type GraphEdge struct {
	From      string  `json:"from"`
	To        string  `json:"to"`
	Amount    float64 `json:"amount"`
	Timestamp int64   `json:"timestamp"`
	TxType    string  `json:"tx_type"` // transfer|dex_swap|deploy
	Weight    float64 `json:"weight"`  // amount * time_decay
}

// EntityReport is the result of querying a deployer's graph profile.
type EntityReport struct {
	DeployerAddress  string  `json:"deployer_address"`
	RiskScore        float64 `json:"risk_score"`   // 0-100
	Labels           []Label `json:"labels"`
	ClusterSize      int     `json:"cluster_size"` // Sybil indicator
	SeedFunder       string  `json:"seed_funder"`
	SeedFunderLabels []Label `json:"seed_funder_labels"`
	HopsToRugger     int     `json:"hops_to_rugger"`  // -1 if none
	HopsToInsider    int     `json:"hops_to_insider"` // -1 if none
	Confidence       float64 `json:"confidence"`      // 0-1
	QueryTimeNs      int64   `json:"query_time_ns"`
}

// Config configures the Entity Graph Engine.
type Config struct {
	MaxNodes           int     `yaml:"max_nodes"`            // max graph nodes (default 500k)
	MaxEdgesPerNode    int     `yaml:"max_edges_per_node"`
	DustThreshold      float64 `yaml:"dust_threshold"`       // ignore edges < this (SOL)
	EvictionDays       int     `yaml:"eviction_days"`        // remove inactive nodes after N days
	LabelPropDepth     int     `yaml:"label_prop_depth"`     // BFS depth for label propagation
	SnapshotIntervalS  int     `yaml:"snapshot_interval_s"`
	TimeDecay7d        float64 `yaml:"time_decay_7d"`        // weight multiplier at 7 days
	TimeDecay30d       float64 `yaml:"time_decay_30d"`       // weight multiplier at 30 days
	SybilClusterMin    int     `yaml:"sybil_cluster_min"`    // min wallets for Sybil flag
	HighTxThreshold    int     `yaml:"high_tx_threshold"`    // ignore edges to popular nodes
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		MaxNodes:          500_000,
		MaxEdgesPerNode:   200,
		DustThreshold:     0.01,  // ignore < 0.01 SOL
		EvictionDays:      30,
		LabelPropDepth:    2,
		SnapshotIntervalS: 300,   // 5 minutes
		TimeDecay7d:       0.5,
		TimeDecay30d:      0.1,
		SybilClusterMin:   20,
		HighTxThreshold:   10_000,
	}
}

// Engine is the in-memory entity graph.
type Engine struct {
	config Config

	mu       sync.RWMutex
	nodes    map[string]*GraphNode
	adjOut   map[string][]GraphEdge // outgoing edges per address
	adjIn    map[string][]GraphEdge // incoming edges per address
	cexAddrs map[string]bool        // known CEX hot wallets

	nextCluster atomic.Uint32

	// Stats.
	nodeCount  atomic.Int64
	edgeCount  atomic.Int64
	queryCount atomic.Int64
}

// NewEngine creates a new Entity Graph Engine.
func NewEngine(config Config) *Engine {
	e := &Engine{
		config:   config,
		nodes:    make(map[string]*GraphNode, config.MaxNodes/10),
		adjOut:   make(map[string][]GraphEdge),
		adjIn:    make(map[string][]GraphEdge),
		cexAddrs: make(map[string]bool),
	}

	// Seed with known CEX hot wallets (Solana).
	for _, addr := range knownCEXWallets {
		e.cexAddrs[addr] = true
		e.nodes[addr] = &GraphNode{
			Address: addr,
			Labels:  []Label{LabelCEX},
			IsCEX:   true,
		}
	}

	return e
}

// ---------------------------------------------------------------------------
// Ingestion — feed events into the graph
// ---------------------------------------------------------------------------

// AddTransfer records a SOL/token transfer between wallets.
func (e *Engine) AddTransfer(from, to string, amount float64, timestamp int64, txType string) {
	// Anti-poisoning: ignore dust.
	if amount < e.config.DustThreshold {
		return
	}

	// CEX firewall: don't link through CEX wallets.
	if e.cexAddrs[from] || e.cexAddrs[to] {
		return
	}

	// Anti-poisoning: ignore edges to popular nodes.
	e.mu.RLock()
	toNode := e.nodes[to]
	e.mu.RUnlock()
	if toNode != nil && toNode.TxCount > uint32(e.config.HighTxThreshold) {
		return
	}

	weight := amount * e.timeDecay(timestamp)

	edge := GraphEdge{
		From:      from,
		To:        to,
		Amount:    amount,
		Timestamp: timestamp,
		TxType:    txType,
		Weight:    weight,
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Ensure nodes exist.
	e.ensureNode(from, timestamp)
	e.ensureNode(to, timestamp)

	// Update tx counts.
	e.nodes[from].TxCount++
	e.nodes[to].TxCount++

	// Add edges (cap per node).
	if len(e.adjOut[from]) < e.config.MaxEdgesPerNode {
		e.adjOut[from] = append(e.adjOut[from], edge)
		e.adjIn[to] = append(e.adjIn[to], edge)
		e.edgeCount.Add(1)
	}
}

// AddDeploy records that an address deployed a token.
func (e *Engine) AddDeploy(deployer string, timestamp int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureNode(deployer, timestamp)
	e.nodes[deployer].DeployCount++
}

// MarkRug marks a deployer as having rugged.
func (e *Engine) MarkRug(deployer string) {
	e.mu.Lock()
	node := e.nodes[deployer]
	if node == nil {
		e.ensureNode(deployer, time.Now().Unix())
		node = e.nodes[deployer]
	}
	node.RugCount++
	if node.RugCount >= 2 {
		e.addLabel(node, LabelSerialRugger)
	}
	e.mu.Unlock()

	// Trigger async label propagation.
	go e.propagateLabels(deployer)
}

// MarkInsider marks a wallet as a profitable insider.
func (e *Engine) MarkInsider(address string) {
	e.mu.Lock()
	node := e.nodes[address]
	if node == nil {
		e.ensureNode(address, time.Now().Unix())
		node = e.nodes[address]
	}
	e.addLabel(node, LabelProfitableInsider)
	e.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Query API — <2ms per call
// ---------------------------------------------------------------------------

// QueryDeployer returns an EntityReport for a deployer address.
func (e *Engine) QueryDeployer(address string) EntityReport {
	start := time.Now()
	e.queryCount.Add(1)

	report := EntityReport{
		DeployerAddress: address,
		HopsToRugger:    -1,
		HopsToInsider:   -1,
		Confidence:      0.0,
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	node := e.nodes[address]
	if node == nil {
		report.RiskScore = 50 // unknown = neutral
		report.Labels = []Label{LabelUnknown}
		report.QueryTimeNs = time.Since(start).Nanoseconds()
		return report
	}

	report.Labels = append([]Label{}, node.Labels...)
	report.ClusterSize = e.clusterSize(address)

	// BFS depth=2 from deployer.
	visited := map[string]int{address: 0} // address -> depth
	queue := []string{address}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		depth := visited[current]

		if depth >= e.config.LabelPropDepth {
			continue
		}

		// Check outgoing edges.
		for _, edge := range e.adjOut[current] {
			if _, seen := visited[edge.To]; seen {
				continue
			}
			visited[edge.To] = depth + 1
			queue = append(queue, edge.To)
		}

		// Check incoming edges (seed funding).
		for _, edge := range e.adjIn[current] {
			if _, seen := visited[edge.From]; seen {
				continue
			}
			visited[edge.From] = depth + 1
			queue = append(queue, edge.From)
		}
	}

	// Analyze BFS results.
	for addr, depth := range visited {
		if addr == address {
			continue
		}
		neighbor := e.nodes[addr]
		if neighbor == nil {
			continue
		}

		for _, label := range neighbor.Labels {
			switch label {
			case LabelSerialRugger:
				if report.HopsToRugger == -1 || depth < report.HopsToRugger {
					report.HopsToRugger = depth
				}
			case LabelProfitableInsider:
				if report.HopsToInsider == -1 || depth < report.HopsToInsider {
					report.HopsToInsider = depth
				}
			}
		}

		// Track seed funder (first incoming transfer).
		if depth == 1 {
			for _, edge := range e.adjIn[address] {
				if edge.From == addr && report.SeedFunder == "" {
					report.SeedFunder = addr
					report.SeedFunderLabels = append([]Label{}, neighbor.Labels...)
				}
			}
		}
	}

	// Calculate risk score.
	report.RiskScore = e.calculateRiskScore(node, report)

	// Calculate confidence based on data quality.
	dataPoints := len(visited) - 1
	if dataPoints > 20 {
		report.Confidence = 0.9
	} else if dataPoints > 5 {
		report.Confidence = 0.7
	} else if dataPoints > 0 {
		report.Confidence = 0.4
	} else {
		report.Confidence = 0.1
	}

	report.QueryTimeNs = time.Since(start).Nanoseconds()
	return report
}

// QueryCluster returns all nodes in a deployer's cluster.
func (e *Engine) QueryCluster(address string) []GraphNode {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var cluster []GraphNode
	visited := map[string]bool{address: true}
	queue := []string{address}

	for len(queue) > 0 && len(cluster) < 100 {
		current := queue[0]
		queue = queue[1:]

		if node := e.nodes[current]; node != nil {
			cluster = append(cluster, *node)
		}

		for _, edge := range e.adjOut[current] {
			if !visited[edge.To] {
				visited[edge.To] = true
				queue = append(queue, edge.To)
			}
		}
		for _, edge := range e.adjIn[current] {
			if !visited[edge.From] {
				visited[edge.From] = true
				queue = append(queue, edge.From)
			}
		}
	}
	return cluster
}

// ---------------------------------------------------------------------------
// Sybil Detection
// ---------------------------------------------------------------------------

// DetectSybilCluster detects if wallets buying the same token in the same block
// with similar parameters are part of a Sybil cluster.
func (e *Engine) DetectSybilCluster(wallets []string, tokenMint string, blockTime int64) bool {
	if len(wallets) < e.config.SybilClusterMin {
		return false
	}

	// Check funding tree: do these wallets share a common funder?
	e.mu.RLock()
	defer e.mu.RUnlock()

	funders := make(map[string]int) // funder -> count of wallets funded
	for _, wallet := range wallets {
		for _, edge := range e.adjIn[wallet] {
			if edge.TxType == "transfer" {
				funders[edge.From]++
			}
		}
	}

	// If one funder funded >50% of the wallets, it's a Sybil cluster.
	threshold := len(wallets) / 2
	for funder, count := range funders {
		if count >= threshold {
			// Mark entire cluster.
			e.mu.RUnlock()
			e.mu.Lock()
			clusterID := e.nextCluster.Add(1)
			for _, wallet := range wallets {
				if node := e.nodes[wallet]; node != nil {
					node.ClusterID = clusterID
					e.addLabel(node, LabelSybilCluster)
				}
			}
			if node := e.nodes[funder]; node != nil {
				node.ClusterID = clusterID
				e.addLabel(node, LabelSybilCluster)
			}
			e.mu.Unlock()
			e.mu.RLock()
			return true
		}
	}
	return false
}

// DetectFundingTree detects if one wallet funded many new wallets recently.
func (e *Engine) DetectFundingTree(funder string, windowHours int) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	cutoff := time.Now().Add(-time.Duration(windowHours) * time.Hour).Unix()
	var funded []string

	for _, edge := range e.adjOut[funder] {
		if edge.TxType == "transfer" && edge.Timestamp > cutoff {
			// Check if recipient is relatively new.
			if node := e.nodes[edge.To]; node != nil {
				if node.TxCount < 5 {
					funded = append(funded, edge.To)
				}
			}
		}
	}
	return funded
}

// ---------------------------------------------------------------------------
// Label Propagation
// ---------------------------------------------------------------------------

func (e *Engine) propagateLabels(source string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	srcNode := e.nodes[source]
	if srcNode == nil {
		return
	}

	// BFS depth=2 propagation.
	visited := map[string]int{source: 0}
	queue := []string{source}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		depth := visited[current]

		if depth >= e.config.LabelPropDepth {
			continue
		}

		// Propagate to neighbors via outgoing edges.
		for _, edge := range e.adjOut[current] {
			if _, seen := visited[edge.To]; seen {
				continue
			}
			visited[edge.To] = depth + 1
			queue = append(queue, edge.To)
		}
	}

	// Apply labels based on depth.
	for addr, depth := range visited {
		if addr == source || depth == 0 {
			continue
		}
		neighbor := e.nodes[addr]
		if neighbor == nil || neighbor.IsCEX {
			continue
		}

		// Counter-propagation: don't flip CLEAN nodes unless 2+ paths confirm.
		if e.hasLabel(neighbor, LabelClean) {
			paths := e.countPaths(source, addr)
			if paths < 2 {
				continue
			}
		}

		// Propagate negative labels (SERIAL_RUGGER).
		for _, label := range srcNode.Labels {
			if label == LabelSerialRugger {
				e.addLabel(neighbor, label)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (e *Engine) ensureNode(address string, timestamp int64) {
	if _, exists := e.nodes[address]; !exists {
		if int64(len(e.nodes)) >= int64(e.config.MaxNodes) {
			return // at capacity
		}
		e.nodes[address] = &GraphNode{
			Address:   address,
			Labels:    []Label{LabelUnknown},
			FirstSeen: timestamp,
			IsCEX:     e.cexAddrs[address],
		}
		e.nodeCount.Add(1)
	}
}

func (e *Engine) addLabel(node *GraphNode, label Label) {
	for _, existing := range node.Labels {
		if existing == label {
			return
		}
	}
	// Remove UNKNOWN if adding a real label.
	if label != LabelUnknown {
		filtered := node.Labels[:0]
		for _, l := range node.Labels {
			if l != LabelUnknown {
				filtered = append(filtered, l)
			}
		}
		node.Labels = filtered
	}
	node.Labels = append(node.Labels, label)
}

func (e *Engine) hasLabel(node *GraphNode, label Label) bool {
	for _, l := range node.Labels {
		if l == label {
			return true
		}
	}
	return false
}

func (e *Engine) clusterSize(address string) int {
	node := e.nodes[address]
	if node == nil || node.ClusterID == 0 {
		return 1
	}
	count := 0
	for _, n := range e.nodes {
		if n.ClusterID == node.ClusterID {
			count++
		}
	}
	return count
}

func (e *Engine) countPaths(from, to string) int {
	// Simple: count direct edges from 'from' to 'to'.
	paths := 0
	for _, edge := range e.adjOut[from] {
		if edge.To == to {
			paths++
		}
	}
	return paths
}

func (e *Engine) timeDecay(timestamp int64) float64 {
	age := time.Since(time.Unix(timestamp, 0))
	days := age.Hours() / 24

	switch {
	case days <= 1:
		return 1.0
	case days <= 7:
		return 1.0 - (1.0-e.config.TimeDecay7d)*(days/7.0)
	case days <= 30:
		return e.config.TimeDecay7d - (e.config.TimeDecay7d-e.config.TimeDecay30d)*((days-7)/23.0)
	default:
		return e.config.TimeDecay30d
	}
}

func (e *Engine) calculateRiskScore(node *GraphNode, report EntityReport) float64 {
	score := 50.0 // neutral baseline

	// Deployer labels.
	for _, label := range node.Labels {
		switch label {
		case LabelSerialRugger:
			return 100.0 // instant max risk
		case LabelWashTrader:
			score += 25
		case LabelSybilCluster:
			score += 20
		case LabelProfitableInsider:
			score -= 20
		case LabelClean:
			score -= 25
		}
	}

	// Proximity to rugger.
	switch report.HopsToRugger {
	case 1:
		score += 40
	case 2:
		score += 20
	}

	// Proximity to insider.
	switch report.HopsToInsider {
	case 1:
		score -= 15
	case 2:
		score -= 8
	}

	// Sybil cluster size.
	if report.ClusterSize > e.config.SybilClusterMin {
		score += 15
	}

	// Seed funder labels.
	for _, label := range report.SeedFunderLabels {
		switch label {
		case LabelSerialRugger:
			score += 30
		case LabelWashTrader:
			score += 15
		case LabelClean:
			score -= 10
		}
	}

	// Deploy frequency.
	if node.DeployCount > 5 {
		score += 15
	}

	// Clamp.
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return score
}

// Evict removes inactive nodes older than eviction threshold.
func (e *Engine) Evict() int {
	cutoff := time.Now().Add(-time.Duration(e.config.EvictionDays) * 24 * time.Hour).Unix()

	e.mu.Lock()
	defer e.mu.Unlock()

	evicted := 0
	for addr, node := range e.nodes {
		if node.IsCEX {
			continue
		}
		if node.FirstSeen < cutoff && node.TxCount == 0 {
			delete(e.nodes, addr)
			delete(e.adjOut, addr)
			delete(e.adjIn, addr)
			evicted++
		}
	}
	e.nodeCount.Add(-int64(evicted))
	return evicted
}

// Stats returns graph statistics.
type GraphStats struct {
	NodeCount  int64 `json:"node_count"`
	EdgeCount  int64 `json:"edge_count"`
	QueryCount int64 `json:"query_count"`
	CEXNodes   int   `json:"cex_nodes"`
}

func (e *Engine) Stats() GraphStats {
	return GraphStats{
		NodeCount:  e.nodeCount.Load(),
		EdgeCount:  e.edgeCount.Load(),
		QueryCount: e.queryCount.Load(),
		CEXNodes:   len(e.cexAddrs),
	}
}

// Known CEX hot wallets (Solana mainnet).
var knownCEXWallets = []string{
	"5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9", // Binance
	"9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM", // FTX (historical)
	"ASTyfSima4LLAdDgoFGkgqoKowG1LZFDr9fAQrg7iaJZ", // Bybit
	"H8sMJSCQxfKiFTCfDR3DUMLPwcRbM61LGFJ8N4dK3WjS", // Coinbase
	"2ojv9BAiHUrvsm9gxDe7fJSzbNZSJcxZvf8dqmWGHG8S", // Kraken
}
