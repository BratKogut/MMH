package graph

import (
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Entity Graph Persistence — gob-encoded snapshots
// Architecture v3.1: snapshot every 5 minutes, load on startup
// ---------------------------------------------------------------------------

// graphSnapshot is the serializable state of the entity graph.
type graphSnapshot struct {
	Nodes       map[string]*GraphNode  `json:"nodes"`
	AdjOut      map[string][]GraphEdge `json:"adj_out"`
	AdjIn       map[string][]GraphEdge `json:"adj_in"`
	NextCluster uint32                 `json:"next_cluster"`
	CreatedAt   time.Time              `json:"created_at"`
	NodeCount   int                    `json:"node_count"`
	EdgeCount   int                    `json:"edge_count"`
}

// SaveSnapshot persists the current graph state to a gob-encoded file.
func (e *Engine) SaveSnapshot(path string) error {
	e.mu.RLock()
	snap := graphSnapshot{
		Nodes:       e.nodes,
		AdjOut:      e.adjOut,
		AdjIn:       e.adjIn,
		NextCluster: e.nextCluster.Load(),
		CreatedAt:   time.Now(),
		NodeCount:   len(e.nodes),
	}
	// Count edges.
	edgeCount := 0
	for _, edges := range e.adjOut {
		edgeCount += len(edges)
	}
	snap.EdgeCount = edgeCount
	e.mu.RUnlock()

	// Write to temp file first, then rename (atomic).
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("graph: create snapshot dir: %w", err)
	}

	tmpPath := path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("graph: create snapshot file: %w", err)
	}

	enc := gob.NewEncoder(f)
	if err := enc.Encode(&snap); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("graph: encode snapshot: %w", err)
	}

	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("graph: close snapshot: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("graph: rename snapshot: %w", err)
	}

	log.Info().
		Int("nodes", snap.NodeCount).
		Int("edges", snap.EdgeCount).
		Str("path", path).
		Msg("graph: snapshot saved")

	return nil
}

// LoadSnapshot restores graph state from a gob-encoded file.
func (e *Engine) LoadSnapshot(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info().Str("path", path).Msg("graph: no snapshot found, starting fresh")
			return nil
		}
		return fmt.Errorf("graph: open snapshot: %w", err)
	}
	defer f.Close()

	var snap graphSnapshot
	dec := gob.NewDecoder(f)
	if err := dec.Decode(&snap); err != nil {
		if err == io.EOF {
			log.Warn().Str("path", path).Msg("graph: empty snapshot, starting fresh")
			return nil
		}
		return fmt.Errorf("graph: decode snapshot: %w", err)
	}

	e.mu.Lock()
	e.nodes = snap.Nodes
	e.adjOut = snap.AdjOut
	e.adjIn = snap.AdjIn
	e.nextCluster.Store(snap.NextCluster)
	// Fix nil maps.
	if e.nodes == nil {
		e.nodes = make(map[string]*GraphNode)
	}
	if e.adjOut == nil {
		e.adjOut = make(map[string][]GraphEdge)
	}
	if e.adjIn == nil {
		e.adjIn = make(map[string][]GraphEdge)
	}
	e.nodeCount.Store(int64(len(e.nodes)))
	e.mu.Unlock()

	log.Info().
		Int("nodes", snap.NodeCount).
		Int("edges", snap.EdgeCount).
		Time("created_at", snap.CreatedAt).
		Str("path", path).
		Msg("graph: snapshot loaded")

	return nil
}

// SnapshotLoop runs periodic snapshots. Blocks until stop channel is closed.
func (e *Engine) SnapshotLoop(path string, interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			// Final snapshot on shutdown.
			if err := e.SaveSnapshot(path); err != nil {
				log.Error().Err(err).Msg("graph: final snapshot failed")
			}
			return
		case <-ticker.C:
			if err := e.SaveSnapshot(path); err != nil {
				log.Error().Err(err).Msg("graph: periodic snapshot failed")
			}
		}
	}
}

// SnapshotInfo returns info about a snapshot file without loading it.
type SnapshotInfo struct {
	Path      string    `json:"path"`
	SizeBytes int64     `json:"size_bytes"`
	ModTime   time.Time `json:"mod_time"`
	Exists    bool      `json:"exists"`
}

func GetSnapshotInfo(path string) SnapshotInfo {
	info, err := os.Stat(path)
	if err != nil {
		return SnapshotInfo{Path: path, Exists: false}
	}
	return SnapshotInfo{
		Path:      path,
		SizeBytes: info.Size(),
		ModTime:   info.ModTime(),
		Exists:    true,
	}
}
