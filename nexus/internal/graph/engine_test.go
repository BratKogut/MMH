package graph

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestEngine() *Engine {
	config := DefaultConfig()
	config.DustThreshold = 0.01
	config.SybilClusterMin = 3
	return NewEngine(config)
}

func TestEngine_NewEngine(t *testing.T) {
	e := newTestEngine()

	stats := e.Stats()
	assert.Greater(t, stats.CEXNodes, 0, "Should seed CEX wallets")

	// CEX nodes are seeded in the map directly.
	e.mu.RLock()
	assert.Greater(t, len(e.nodes), 0, "Should have seeded CEX nodes in map")
	e.mu.RUnlock()
}

func TestEngine_AddTransfer(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()
	e.AddTransfer("walletA", "walletB", 1.0, ts, "transfer")

	e.mu.RLock()
	assert.NotNil(t, e.nodes["walletA"])
	assert.NotNil(t, e.nodes["walletB"])
	assert.Len(t, e.adjOut["walletA"], 1)
	assert.Len(t, e.adjIn["walletB"], 1)
	e.mu.RUnlock()

	stats := e.Stats()
	assert.Equal(t, int64(1), stats.EdgeCount)
}

func TestEngine_DustFilter(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()
	e.AddTransfer("walletA", "walletB", 0.001, ts, "transfer") // dust

	e.mu.RLock()
	assert.Nil(t, e.nodes["walletA"], "Dust transfer should be ignored")
	e.mu.RUnlock()
}

func TestEngine_CEXFirewall(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()
	cex := knownCEXWallets[0]
	e.AddTransfer("walletA", cex, 10.0, ts, "transfer")

	e.mu.RLock()
	// CEX node exists from seeding, but walletA should not be created.
	assert.Nil(t, e.nodes["walletA"], "Transfer to CEX should be blocked")
	e.mu.RUnlock()
}

func TestEngine_MarkRug(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()
	e.AddDeploy("deployer1", ts)
	e.MarkRug("deployer1")
	e.MarkRug("deployer1") // 2nd rug = SERIAL_RUGGER

	// Wait for async label propagation.
	time.Sleep(10 * time.Millisecond)

	e.mu.RLock()
	node := e.nodes["deployer1"]
	require.NotNil(t, node)
	assert.Equal(t, 2, node.RugCount)
	assert.True(t, e.hasLabel(node, LabelSerialRugger))
	e.mu.RUnlock()
}

func TestEngine_MarkInsider(t *testing.T) {
	e := newTestEngine()

	e.MarkInsider("insider1")

	e.mu.RLock()
	node := e.nodes["insider1"]
	require.NotNil(t, node)
	assert.True(t, e.hasLabel(node, LabelProfitableInsider))
	e.mu.RUnlock()
}

func TestEngine_QueryDeployer_Unknown(t *testing.T) {
	e := newTestEngine()

	report := e.QueryDeployer("unknown-wallet")

	assert.Equal(t, "unknown-wallet", report.DeployerAddress)
	assert.Equal(t, 50.0, report.RiskScore)
	assert.Equal(t, -1, report.HopsToRugger)
	assert.Equal(t, -1, report.HopsToInsider)
	assert.Contains(t, report.Labels, LabelUnknown)
}

func TestEngine_QueryDeployer_NearRugger(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()

	// Create a rugger.
	e.AddDeploy("rugger", ts)
	e.MarkRug("rugger")
	e.MarkRug("rugger")
	time.Sleep(10 * time.Millisecond)

	// Fund deployer from rugger.
	e.AddTransfer("rugger", "deployer", 5.0, ts, "transfer")

	report := e.QueryDeployer("deployer")

	assert.Equal(t, 1, report.HopsToRugger, "Should be 1 hop from rugger")
	assert.Greater(t, report.RiskScore, 70.0, "Should have high risk score")
}

func TestEngine_QueryDeployer_NearInsider(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()

	e.MarkInsider("insider")
	e.AddTransfer("insider", "deployer", 3.0, ts, "transfer")

	report := e.QueryDeployer("deployer")

	assert.Equal(t, 1, report.HopsToInsider)
	assert.Less(t, report.RiskScore, 50.0, "Should have lower risk near insider")
}

func TestEngine_QueryDeployer_SeedFunder(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()
	e.AddTransfer("funder", "deployer", 2.0, ts, "transfer")

	report := e.QueryDeployer("deployer")

	assert.Equal(t, "funder", report.SeedFunder)
}

func TestEngine_DetectSybilCluster(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()

	// Create common funder.
	wallets := []string{"w1", "w2", "w3", "w4"}
	for _, w := range wallets {
		e.AddTransfer("funder", w, 1.0, ts, "transfer")
	}

	detected := e.DetectSybilCluster(wallets, "token1", ts)
	assert.True(t, detected, "Should detect Sybil cluster")

	e.mu.RLock()
	for _, w := range wallets {
		node := e.nodes[w]
		if node != nil {
			assert.True(t, e.hasLabel(node, LabelSybilCluster))
		}
	}
	e.mu.RUnlock()
}

func TestEngine_DetectFundingTree(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()

	// Funder sends to 3 new wallets recently.
	e.AddTransfer("funder", "new1", 1.0, ts, "transfer")
	e.AddTransfer("funder", "new2", 1.0, ts, "transfer")
	e.AddTransfer("funder", "new3", 1.0, ts, "transfer")

	funded := e.DetectFundingTree("funder", 24)
	assert.Len(t, funded, 3)
}

func TestEngine_QueryCluster(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()
	e.AddTransfer("a", "b", 1.0, ts, "transfer")
	e.AddTransfer("b", "c", 1.0, ts, "transfer")

	cluster := e.QueryCluster("a")
	assert.GreaterOrEqual(t, len(cluster), 3)
}

func TestEngine_TimeDecay(t *testing.T) {
	e := newTestEngine()

	now := time.Now().Unix()
	assert.InDelta(t, 1.0, e.timeDecay(now), 0.01)

	sevenDaysAgo := time.Now().Add(-7 * 24 * time.Hour).Unix()
	assert.InDelta(t, 0.5, e.timeDecay(sevenDaysAgo), 0.1)

	thirtyDaysAgo := time.Now().Add(-30 * 24 * time.Hour).Unix()
	assert.InDelta(t, 0.1, e.timeDecay(thirtyDaysAgo), 0.1)
}

func TestEngine_Evict(t *testing.T) {
	e := newTestEngine()

	// Add a node with old timestamp and no txs.
	e.mu.Lock()
	e.nodes["old-wallet"] = &GraphNode{
		Address:   "old-wallet",
		Labels:    []Label{LabelUnknown},
		FirstSeen: time.Now().Add(-60 * 24 * time.Hour).Unix(), // 60 days ago
		TxCount:   0,
	}
	e.nodeCount.Add(1)
	e.mu.Unlock()

	evicted := e.Evict()
	assert.Equal(t, 1, evicted)

	e.mu.RLock()
	assert.Nil(t, e.nodes["old-wallet"])
	e.mu.RUnlock()
}

func TestEngine_LabelPropagation(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()

	// Create rugger -> neighbor chain.
	e.AddDeploy("rugger", ts)
	e.AddTransfer("rugger", "neighbor", 5.0, ts, "transfer")

	// Mark rugger twice to make SERIAL_RUGGER — triggers propagation.
	e.MarkRug("rugger")
	e.MarkRug("rugger")

	// Wait for async propagation.
	time.Sleep(50 * time.Millisecond)

	e.mu.RLock()
	neighbor := e.nodes["neighbor"]
	require.NotNil(t, neighbor)
	assert.True(t, e.hasLabel(neighbor, LabelSerialRugger),
		"SERIAL_RUGGER should propagate to 1-hop neighbor")
	e.mu.RUnlock()
}

func TestEngine_HighTxFilter(t *testing.T) {
	e := newTestEngine()

	ts := time.Now().Unix()

	// Create a popular node above threshold.
	e.mu.Lock()
	e.nodes["popular"] = &GraphNode{
		Address:   "popular",
		Labels:    []Label{LabelUnknown},
		FirstSeen: ts,
		TxCount:   15000, // above HighTxThreshold
	}
	e.mu.Unlock()

	e.AddTransfer("walletX", "popular", 5.0, ts, "transfer")

	e.mu.RLock()
	// walletX should not be created because transfer to popular node is filtered.
	assert.Nil(t, e.nodes["walletX"])
	e.mu.RUnlock()
}
