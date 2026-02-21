#!/bin/bash
# Provision Kafka topics in RedPanda
# Usage: ./scripts/provision-topics.sh

set -euo pipefail

BROKER="${KAFKA_BROKER:-localhost:19092}"
RPK="rpk -X brokers=$BROKER"

echo "Provisioning NEXUS Kafka topics on $BROKER..."

# Market Data topics
$RPK topic create md.heartbeat -p 1 -r 1 -c retention.ms=86400000
$RPK topic create md.ticks.kraken.btcusd -p 1 -r 1 -c retention.ms=604800000
$RPK topic create md.ticks.kraken.ethusd -p 1 -r 1 -c retention.ms=604800000
$RPK topic create md.ticks.binance.btcusdt -p 1 -r 1 -c retention.ms=604800000
$RPK topic create md.ticks.binance.ethusdt -p 1 -r 1 -c retention.ms=604800000
$RPK topic create md.trades.kraken.btcusd -p 1 -r 1 -c retention.ms=604800000
$RPK topic create md.trades.kraken.ethusd -p 1 -r 1 -c retention.ms=604800000
$RPK topic create md.trades.binance.btcusdt -p 1 -r 1 -c retention.ms=604800000
$RPK topic create md.trades.binance.ethusdt -p 1 -r 1 -c retention.ms=604800000
$RPK topic create md.orderbook_l2.kraken.btcusd -p 1 -r 1 -c retention.ms=259200000
$RPK topic create md.orderbook_l2.kraken.ethusd -p 1 -r 1 -c retention.ms=259200000
$RPK topic create md.orderbook_l2.binance.btcusdt -p 1 -r 1 -c retention.ms=259200000
$RPK topic create md.orderbook_l2.binance.ethusdt -p 1 -r 1 -c retention.ms=259200000
$RPK topic create md.ohlcv_1m.kraken.btcusd -p 1 -r 1 -c retention.ms=2592000000
$RPK topic create md.ohlcv_1m.kraken.ethusd -p 1 -r 1 -c retention.ms=2592000000
$RPK topic create md.ohlcv_1m.binance.btcusdt -p 1 -r 1 -c retention.ms=2592000000
$RPK topic create md.ohlcv_1m.binance.ethusdt -p 1 -r 1 -c retention.ms=2592000000

# Execution topics
$RPK topic create exec.orders.kraken -p 1 -r 1 -c retention.ms=2592000000
$RPK topic create exec.orders.binance -p 1 -r 1 -c retention.ms=2592000000
$RPK topic create exec.acks.kraken -p 1 -r 1 -c retention.ms=2592000000
$RPK topic create exec.acks.binance -p 1 -r 1 -c retention.ms=2592000000
$RPK topic create exec.fills.kraken -p 3 -r 1 -c retention.ms=7776000000
$RPK topic create exec.fills.binance -p 3 -r 1 -c retention.ms=7776000000
$RPK topic create exec.positions.kraken -p 1 -r 1 -c retention.ms=7776000000
$RPK topic create exec.positions.binance -p 1 -r 1 -c retention.ms=7776000000
$RPK topic create exec.balances.kraken -p 1 -r 1 -c retention.ms=7776000000
$RPK topic create exec.balances.binance -p 1 -r 1 -c retention.ms=7776000000

# Risk
$RPK topic create risk.pretrade_checks -p 3 -r 1 -c retention.ms=2592000000

# Signals
$RPK topic create signals.features.btcusd -p 1 -r 1 -c retention.ms=2592000000
$RPK topic create signals.features.ethusd -p 1 -r 1 -c retention.ms=2592000000
$RPK topic create signals.regime.btcusd -p 1 -r 1 -c retention.ms=2592000000
$RPK topic create signals.regime.ethusd -p 1 -r 1 -c retention.ms=2592000000

# Intel
$RPK topic create intel.events.global -p 3 -r 1 -c retention.ms=7776000000
$RPK topic create intel.alerts -p 1 -r 1 -c retention.ms=2592000000
$RPK topic create intel.raw.grok -p 1 -r 1 -c retention.ms=259200000
$RPK topic create intel.raw.claude -p 1 -r 1 -c retention.ms=259200000
$RPK topic create intel.raw.internal_llm -p 1 -r 1 -c retention.ms=259200000

# Ops
$RPK topic create ops.logs.core -p 3 -r 1 -c retention.ms=604800000
$RPK topic create ops.alerts.core -p 1 -r 1 -c retention.ms=2592000000

# Audit (source of truth - 1 year retention)
$RPK topic create audit.event_store -p 6 -r 1 -c retention.ms=31536000000

echo ""
echo "All topics provisioned. Listing:"
$RPK topic list
