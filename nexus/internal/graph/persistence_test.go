package graph

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEngine_SaveAndLoadSnapshot(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "graph_snapshot.gob")

	// Build a graph.
	e := NewEngine(DefaultConfig())
	now := time.Now().Unix()
	e.AddTransfer("wallet1", "wallet2", 5.0, now, "transfer")
	e.MarkRug("wallet2")
	e.MarkRug("wallet2") // 2x to get SerialRugger label

	// Verify user nodes were added (counter tracks AddTransfer additions only).
	stats := e.Stats()
	assert.True(t, stats.NodeCount >= 2)

	// Save snapshot.
	err := e.SaveSnapshot(path)
	require.NoError(t, err)

	// Verify file exists.
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.Size() > 0)

	// Create new engine and load.
	e2 := NewEngine(DefaultConfig())
	err = e2.LoadSnapshot(path)
	require.NoError(t, err)

	// Verify restored state (includes CEX seed nodes from original engine).
	stats2 := e2.Stats()
	assert.True(t, stats2.NodeCount >= 2)

	// Verify nodes and labels preserved.
	report := e2.QueryDeployer("wallet2")
	assert.Contains(t, report.Labels, LabelSerialRugger)
}

func TestEngine_LoadSnapshot_NotFound(t *testing.T) {
	e := NewEngine(DefaultConfig())
	err := e.LoadSnapshot("/nonexistent/path/snap.gob")
	assert.NoError(t, err) // Should not error, just start fresh.
}

func TestEngine_LoadSnapshot_Empty(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "empty.gob")
	os.WriteFile(path, []byte{}, 0o644)

	e := NewEngine(DefaultConfig())
	err := e.LoadSnapshot(path)
	assert.NoError(t, err)
}

func TestGetSnapshotInfo(t *testing.T) {
	// Non-existent.
	info := GetSnapshotInfo("/nonexistent/file")
	assert.False(t, info.Exists)

	// Existing.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.gob")
	os.WriteFile(path, []byte("test"), 0o644)

	info = GetSnapshotInfo(path)
	assert.True(t, info.Exists)
	assert.Equal(t, int64(4), info.SizeBytes)
}
