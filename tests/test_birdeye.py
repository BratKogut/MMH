"""Tests for Birdeye security provider."""

import pytest
from unittest.mock import AsyncMock, MagicMock, patch
from src.enrichment.providers.birdeye import BirdeyeProvider


@pytest.fixture
def provider():
    return BirdeyeProvider(api_key="test-key", rate_limit_per_sec=100.0)


class TestBirdeyeProvider:
    """Tests for BirdeyeProvider."""

    @pytest.mark.asyncio
    async def test_provider_name(self, provider):
        assert provider.name == "birdeye"

    @pytest.mark.asyncio
    async def test_analyze_flags_safe_token(self, provider):
        data = {
            "lpBurnedPercent": "95.0",
            "top10HolderPercent": "30.0",
            "creatorPercentage": "5.0",
            "isMintable": False,
            "isFreezable": False,
        }
        flags = provider._analyze_flags(data)
        assert not any("CRITICAL" in f for f in flags)
        assert not any("HIGH" in f for f in flags)

    @pytest.mark.asyncio
    async def test_analyze_flags_dangerous_token(self, provider):
        data = {
            "lpBurnedPercent": "0",
            "top10HolderPercent": "85.0",
            "creatorPercentage": "25.0",
            "isMintable": True,
            "isFreezable": True,
        }
        flags = provider._analyze_flags(data)
        assert "CRITICAL:MINTABLE" in flags
        assert "CRITICAL:HIGH_HOLDER_CONCENTRATION" in flags
        assert "HIGH:LP_NOT_BURNED" in flags
        assert "HIGH:FREEZABLE" in flags
        assert "HIGH:CREATOR_HOLDS_LARGE_SUPPLY" in flags

    @pytest.mark.asyncio
    async def test_analyze_flags_low_lp_burn(self, provider):
        data = {"lpBurnedPercent": "30.0"}
        flags = provider._analyze_flags(data)
        assert "LOW_LP_BURN" in flags

    @pytest.mark.asyncio
    async def test_determine_risk_level_critical(self, provider):
        flags = ["CRITICAL:MINTABLE", "LOW_LP_BURN"]
        assert provider._determine_risk_level(flags) == "CRITICAL"

    @pytest.mark.asyncio
    async def test_determine_risk_level_high(self, provider):
        flags = ["HIGH:FREEZABLE", "LOW_LP_BURN"]
        assert provider._determine_risk_level(flags) == "HIGH"

    @pytest.mark.asyncio
    async def test_determine_risk_level_medium(self, provider):
        flags = ["LOW_LP_BURN"]
        assert provider._determine_risk_level(flags) == "MEDIUM"

    @pytest.mark.asyncio
    async def test_determine_risk_level_low(self, provider):
        flags = []
        assert provider._determine_risk_level(flags) == "LOW"

    @pytest.mark.asyncio
    async def test_check_handles_exception(self, provider):
        """Provider should return UNKNOWN on error, not crash."""
        with patch.object(provider, '_get_session', side_effect=Exception("connection failed")):
            result = await provider.check("solana", "FakeToken111111111111111111111111111111")
            assert result.risk_level == "UNKNOWN"
            assert "PROVIDER_ERROR" in result.flags
            assert result.is_safe is None

    @pytest.mark.asyncio
    async def test_stats_tracking(self, provider):
        """Provider tracks call and error counts."""
        with patch.object(provider, '_get_session', side_effect=Exception("fail")):
            await provider.check("solana", "test")
        stats = provider.get_stats()
        assert stats["calls"] == 1
        assert stats["errors"] == 1
