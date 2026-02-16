"""Prometheus Metrics - Production Pack.

Minimum metrics that protect capital:
- queue lag per stream/consumer group
- DLQ rate + top reasons
- scoring latency P95/P99
- risk rejects by reason
- execution fail rate by reason (revert/timeout/nonce)
- exposure gauges (per chain/token)
- kill/freeze state gauge
- replay divergence detector (hash of core outputs)
"""

import logging
from typing import Optional

logger = logging.getLogger(__name__)

try:
    from prometheus_client import (
        Counter, Gauge, Histogram, Info,
        start_http_server, CollectorRegistry, REGISTRY
    )
    PROMETHEUS_AVAILABLE = True
except ImportError:
    PROMETHEUS_AVAILABLE = False
    logger.warning("prometheus_client not available, metrics disabled")


class MetricsCollector:
    """Centralized Prometheus metrics collector."""

    def __init__(self, port: int = 8000):
        self._port = port
        self._started = False

        if not PROMETHEUS_AVAILABLE:
            return

        # === Stream/Queue Metrics ===
        self.stream_lag = Gauge(
            'mmh_stream_lag',
            'Consumer group lag (pending messages)',
            ['stream', 'group']
        )

        self.stream_processed = Counter(
            'mmh_stream_processed_total',
            'Total messages processed',
            ['stream', 'group']
        )

        # === DLQ Metrics ===
        self.dlq_size = Gauge(
            'mmh_dlq_size',
            'Dead letter queue size',
            ['source_stream']
        )

        self.dlq_additions = Counter(
            'mmh_dlq_additions_total',
            'Total messages sent to DLQ',
            ['source_stream', 'reason']
        )

        # === Scoring Metrics ===
        self.scoring_latency = Histogram(
            'mmh_scoring_latency_seconds',
            'Scoring processing latency',
            ['chain'],
            buckets=[0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0]
        )

        self.scoring_results = Counter(
            'mmh_scoring_results_total',
            'Scoring results by recommendation',
            ['chain', 'recommendation']
        )

        # === Risk Metrics ===
        self.risk_decisions = Counter(
            'mmh_risk_decisions_total',
            'Risk decisions by type',
            ['decision']
        )

        self.risk_rejects_by_reason = Counter(
            'mmh_risk_rejects_total',
            'Risk rejections by reason',
            ['reason']
        )

        # === Execution Metrics ===
        self.execution_attempts = Counter(
            'mmh_execution_attempts_total',
            'Execution attempts',
            ['chain', 'status']
        )

        self.execution_failures_by_reason = Counter(
            'mmh_execution_failures_total',
            'Execution failures by reason',
            ['chain', 'reason']
        )

        self.execution_latency = Histogram(
            'mmh_execution_latency_seconds',
            'Execution latency',
            ['chain'],
            buckets=[0.1, 0.5, 1.0, 2.0, 5.0, 10.0, 30.0, 60.0]
        )

        # === Exposure Gauges ===
        self.exposure_usd = Gauge(
            'mmh_exposure_usd',
            'Current exposure in USD',
            ['chain']
        )

        self.exposure_total_usd = Gauge(
            'mmh_exposure_total_usd',
            'Total portfolio exposure in USD'
        )

        self.open_positions = Gauge(
            'mmh_open_positions',
            'Number of open positions',
            ['chain']
        )

        # === System State ===
        self.system_state = Gauge(
            'mmh_system_state',
            'System state (1=running, 0=frozen, -1=killed)'
        )

        self.chain_frozen = Gauge(
            'mmh_chain_frozen',
            'Chain freeze state (1=frozen, 0=running)',
            ['chain']
        )

        # === PnL ===
        self.daily_pnl_usd = Gauge(
            'mmh_daily_pnl_usd',
            'Daily PnL in USD'
        )

        self.total_pnl_usd = Gauge(
            'mmh_total_pnl_usd',
            'Total PnL in USD'
        )

        # === Collector Metrics ===
        self.events_collected = Counter(
            'mmh_events_collected_total',
            'Total events collected',
            ['chain', 'source']
        )

        self.wal_writes = Counter(
            'mmh_wal_writes_total',
            'Total WAL writes',
            ['chain']
        )

        self.reorgs_detected = Counter(
            'mmh_reorgs_detected_total',
            'Blockchain reorgs detected',
            ['chain']
        )

        # === Circuit Breaker ===
        self.circuit_breaker_state = Gauge(
            'mmh_circuit_breaker_state',
            'Circuit breaker state (0=closed, 1=open, 2=half_open)',
            ['component']
        )

        # === Heartbeat ===
        self.heartbeat_age = Gauge(
            'mmh_heartbeat_age_seconds',
            'Time since last heartbeat',
            ['module']
        )

        # === Replay ===
        self.replay_divergence = Counter(
            'mmh_replay_divergence_total',
            'Replay divergence events detected'
        )

        # === Build Info ===
        self.build_info = Info(
            'mmh_build',
            'Build information'
        )

    def start_server(self):
        """Start Prometheus HTTP server."""
        if not PROMETHEUS_AVAILABLE or self._started:
            return
        try:
            start_http_server(self._port)
            self._started = True
            logger.info(f"Prometheus metrics server started on port {self._port}")
        except Exception as e:
            logger.error(f"Failed to start metrics server: {e}")

    def set_build_info(self, version: str, instance_id: str, environment: str):
        """Set build information."""
        if PROMETHEUS_AVAILABLE:
            self.build_info.info({
                'version': version,
                'instance_id': instance_id,
                'environment': environment,
            })


# Singleton
_metrics: Optional[MetricsCollector] = None

def get_metrics(port: int = 8000) -> MetricsCollector:
    global _metrics
    if _metrics is None:
        _metrics = MetricsCollector(port)
    return _metrics
