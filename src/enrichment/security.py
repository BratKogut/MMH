"""Enrichment and Security Service.

2-tier security validation:
1. Cached pre-filter (fast, 30s TTL) - for discovery alerts
2. Fresh pre-execution (mandatory, bypasses cache) - before trades

CRITICAL: Every enrichment/security result is logged as an event for replay determinism.
"""

import asyncio
import logging
import time
import json
import hashlib
from typing import Optional

logger = logging.getLogger(__name__)


class SecurityCache:
    """In-memory + Redis cache for security results.
    TTL: 30 seconds for pre-filter, 0 for pre-execution.
    """

    def __init__(self, redis_client, default_ttl: int = 30):
        self._redis = redis_client
        self._default_ttl = default_ttl

    async def get(self, chain: str, token_address: str) -> Optional[dict]:
        """Get cached security result."""
        key = f"security_cache:{chain}:{token_address}"
        data = await self._redis.get(key)
        if data:
            return json.loads(data)
        return None

    async def set(self, chain: str, token_address: str, result: dict, ttl: int = None):
        """Cache security result."""
        key = f"security_cache:{chain}:{token_address}"
        await self._redis.set(key, json.dumps(result), ex=ttl or self._default_ttl)

    async def invalidate(self, chain: str, token_address: str):
        """Invalidate cache entry."""
        key = f"security_cache:{chain}:{token_address}"
        await self._redis.delete(key)


class EnrichmentService:
    """Enrichment and Security service.

    Consumes from: tokens:sanitized:{chain}
    Publishes to: tokens:enriched:{chain}
    Also publishes to: journal:decisions (for replay)

    2-tier approach:
    - Tier 1 (cached): Fast pre-filter during discovery
    - Tier 2 (fresh): Mandatory pre-execution check
    """

    def __init__(self, bus, redis_client, providers: dict, enabled_chains: list[str]):
        """
        Args:
            bus: RedisEventBus instance
            redis_client: Redis client for caching
            providers: dict mapping chain -> list of BaseSecurityProvider
            enabled_chains: list of chain names
        """
        self._bus = bus
        self._redis = redis_client
        self._providers = providers  # {"solana": [birdeye], "base": [goplus], ...}
        self._chains = enabled_chains
        self._cache = SecurityCache(redis_client)
        self._running = False

        # Metrics
        self._checks_total = 0
        self._checks_cached = 0
        self._checks_fresh = 0
        self._checks_failed = 0

    async def start(self):
        """Start enrichment consumers for all chains."""
        self._running = True
        tasks = []
        for chain in self._chains:
            tasks.append(asyncio.create_task(self._consume_chain(chain)))
        await asyncio.gather(*tasks)

    async def stop(self):
        self._running = False
        for chain_providers in self._providers.values():
            for provider in chain_providers:
                if hasattr(provider, 'close'):
                    await provider.close()

    async def _consume_chain(self, chain: str):
        """Consume sanitized events and enrich them."""
        input_stream = f"tokens:sanitized:{chain}"
        output_stream = f"tokens:enriched:{chain}"
        group = "enrichment"
        consumer = f"enrichment-{chain}"

        await self._bus.ensure_consumer_group(input_stream, group)

        while self._running:
            try:
                messages = await self._bus.consume(input_stream, group, consumer, count=5, block_ms=2000)
                for msg_id, data in messages:
                    token_address = data.get("address", "") or data.get("token_address", "")

                    # Tier 1: Cached pre-filter
                    result = await self.check_cached(chain, token_address)

                    if result and result.get("is_safe") is False:
                        logger.info(f"Token {token_address} on {chain} rejected by cached security: {result.get('flags')}")
                        await self._bus.ack(input_stream, group, msg_id)
                        continue

                    # Enrich the event with security data
                    enriched = dict(data)
                    enriched["security_result"] = json.dumps(result) if result else "{}"
                    enriched["enriched_at"] = str(time.time())

                    await self._bus.publish(output_stream, enriched)

                    # Log to decision journal for replay
                    await self._log_decision(chain, token_address, result, data.get("event_id", ""))

                    await self._bus.ack(input_stream, group, msg_id)
            except Exception as e:
                logger.error(f"Enrichment error on {chain}: {e}")
                await asyncio.sleep(1)

    async def check_cached(self, chain: str, token_address: str) -> Optional[dict]:
        """Tier 1: Cached security check (fast pre-filter)."""
        self._checks_total += 1

        # Try cache first
        cached = await self._cache.get(chain, token_address)
        if cached:
            self._checks_cached += 1
            cached["check_type"] = "cached"
            return cached

        # Cache miss - do fresh check and cache result
        result = await self._do_check(chain, token_address)
        if result:
            await self._cache.set(chain, token_address, result)
            result["check_type"] = "cached"
        return result

    async def check_fresh(self, chain: str, token_address: str) -> Optional[dict]:
        """Tier 2: Fresh security check (pre-execution, bypasses cache).

        MANDATORY before any trade execution.
        """
        self._checks_total += 1
        self._checks_fresh += 1

        # Always bypass cache
        result = await self._do_check(chain, token_address)
        if result:
            result["check_type"] = "fresh"
            # Update cache too
            await self._cache.set(chain, token_address, result)

            # Log for replay
            await self._log_decision(chain, token_address, result, "fresh_preexec")
        return result

    async def _do_check(self, chain: str, token_address: str) -> Optional[dict]:
        """Execute actual security checks against providers."""
        providers = self._providers.get(chain, [])
        if not providers:
            logger.warning(f"No security providers for chain {chain}")
            return None

        all_flags = []
        worst_risk = "LOW"
        is_safe = True
        all_data = {}
        risk_hierarchy = {"LOW": 0, "MEDIUM": 1, "HIGH": 2, "CRITICAL": 3, "UNKNOWN": -1}

        for provider in providers:
            try:
                result = await provider.check(chain, token_address)
                all_data[provider.name] = result.data
                all_flags.extend(result.flags)

                if result.is_safe is False:
                    is_safe = False

                if risk_hierarchy.get(result.risk_level, -1) > risk_hierarchy.get(worst_risk, 0):
                    worst_risk = result.risk_level

            except Exception as e:
                logger.error(f"Provider {provider.name} failed for {token_address}: {e}")
                self._checks_failed += 1
                all_flags.append(f"PROVIDER_ERROR:{provider.name}")

        return {
            "chain": chain,
            "token_address": token_address,
            "is_safe": is_safe,
            "risk_level": worst_risk,
            "flags": all_flags,
            "providers_data": all_data,
            "checked_at": time.time(),
        }

    async def _log_decision(self, chain: str, token_address: str, result: Optional[dict], trigger_event_id: str):
        """Log security decision to journal stream for replay determinism."""
        journal_entry = {
            "event_type": "DecisionJournalEntry",
            "decision_type": "SECURITY",
            "module": "EnrichmentService",
            "chain": chain,
            "token_address": token_address,
            "input_event_ids": json.dumps([trigger_event_id]),
            "result_summary": json.dumps({
                "is_safe": result.get("is_safe") if result else None,
                "risk_level": result.get("risk_level") if result else "UNKNOWN",
                "flags": result.get("flags", []) if result else [],
                "check_type": result.get("check_type", "unknown") if result else "unknown",
            }),
            "event_time": str(time.time()),
        }
        try:
            await self._bus.publish("journal:decisions", journal_entry)
        except Exception as e:
            logger.error(f"Failed to log security decision: {e}")

    def get_metrics(self) -> dict:
        return {
            "checks_total": self._checks_total,
            "checks_cached": self._checks_cached,
            "checks_fresh": self._checks_fresh,
            "checks_failed": self._checks_failed,
            "provider_stats": {
                chain: [p.get_stats() for p in providers]
                for chain, providers in self._providers.items()
            }
        }
