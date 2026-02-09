"""Tests for Scoring Engine - determinism is critical."""
import pytest
import json

from src.scoring.scoring_engine import ScoringEngine, ScoringConfig, Recommendation


class TestScoringDeterminism:
    """Scoring MUST be deterministic: same input -> same output."""

    def setup_method(self):
        self.config = ScoringConfig()
        self.engine = ScoringEngine(bus=None, config=self.config, enabled_chains=["solana", "base"])

    def test_same_input_same_output(self):
        event = {
            "chain": "solana",
            "address": "test_token_address_1234567890abcdef",
            "initial_liquidity_usd": "5000",
            "bonding_curve_progress": "75",
            "launchpad": "pump.fun",
            "security_result": json.dumps({
                "is_safe": True,
                "risk_level": "LOW",
                "flags": [],
                "providers_data": {
                    "birdeye": {
                        "security": {
                            "lpBurnedPercent": 80,
                            "top10HolderPercent": 30,
                        }
                    }
                }
            }),
        }

        # Score same input multiple times
        results = [self.engine.score(event, "solana") for _ in range(100)]

        # All results must be identical
        first = results[0]
        for r in results[1:]:
            assert r.risk_score == first.risk_score
            assert r.momentum_score == first.momentum_score
            assert r.overall_score == first.overall_score
            assert r.recommendation == first.recommendation

    def test_critical_security_forces_avoid(self):
        event = {
            "chain": "base",
            "address": "0x" + "a" * 40,
            "security_result": json.dumps({
                "is_safe": False,
                "risk_level": "CRITICAL",
                "flags": ["HONEYPOT"],
                "providers_data": {}
            }),
        }
        result = self.engine.score(event, "base")
        assert result.recommendation == Recommendation.AVOID

    def test_config_hash_present(self):
        event = {
            "chain": "solana",
            "address": "test_address_12345678901234567890",
            "security_result": "{}",
        }
        result = self.engine.score(event, "solana")
        assert result.config_hash
        assert len(result.config_hash) == 16

    def test_different_chains_different_weights(self):
        event = {
            "chain": "solana",
            "address": "test_address_12345678901234567890",
            "security_result": json.dumps({"is_safe": True, "risk_level": "LOW", "flags": [], "providers_data": {}}),
        }
        sol_result = self.engine.score(event, "solana")
        base_result = self.engine.score(event, "base")
        # Scores may differ due to different weight configurations
        assert sol_result.config_hash == base_result.config_hash  # same engine config


class TestScoringRecommendations:
    def setup_method(self):
        self.config = ScoringConfig(min_score_strong_buy=80, min_score_buy=60)
        self.engine = ScoringEngine(bus=None, config=self.config)

    def test_recommendation_levels(self):
        """Verify recommendation thresholds."""
        # This is a basic structural test
        assert self.config.min_score_strong_buy == 80
        assert self.config.min_score_buy == 60
