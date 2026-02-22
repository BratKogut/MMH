"""Tests for GoPlus security provider."""

import pytest
from unittest.mock import AsyncMock, patch
from src.enrichment.providers.goplus import GoPlusProvider


@pytest.fixture
def provider():
    return GoPlusProvider(rate_limit_per_sec=100.0)


class TestGoPlusProvider:
    """Tests for GoPlusProvider."""

    @pytest.mark.asyncio
    async def test_provider_name(self, provider):
        assert provider.name == "goplus"

    @pytest.mark.asyncio
    async def test_unsupported_chain(self, provider):
        result = await provider.check("unknown_chain", "0x123")
        assert result.risk_level == "UNKNOWN"
        assert "UNSUPPORTED_CHAIN" in result.flags

    @pytest.mark.asyncio
    async def test_analyze_flags_honeypot(self, provider):
        data = {"is_honeypot": "1"}
        flags = provider._analyze_flags(data)
        assert "HONEYPOT" in flags

    @pytest.mark.asyncio
    async def test_analyze_flags_safe_token(self, provider):
        data = {
            "is_honeypot": "0",
            "cannot_sell_all": "0",
            "is_mintable": "0",
            "owner_change_balance": "0",
            "is_proxy": "0",
            "buy_tax": "0.01",
            "sell_tax": "0.02",
            "holder_count": "500",
            "is_open_source": "1",
        }
        flags = provider._analyze_flags(data)
        assert "HONEYPOT" not in flags
        assert not any("CRITICAL" in f for f in flags)

    @pytest.mark.asyncio
    async def test_analyze_flags_dangerous_token(self, provider):
        data = {
            "is_honeypot": "0",
            "cannot_sell_all": "1",
            "is_mintable": "1",
            "owner_change_balance": "1",
            "is_proxy": "1",
            "hidden_owner": "1",
            "can_take_back_ownership": "1",
            "buy_tax": "0.15",
            "sell_tax": "0.25",
        }
        flags = provider._analyze_flags(data)
        assert "CRITICAL:CANNOT_SELL" in flags
        assert "CRITICAL:MINTABLE" in flags
        assert "CRITICAL:OWNER_CAN_DRAIN" in flags
        assert "HIGH:PROXY_CONTRACT" in flags
        assert "HIGH:HIDDEN_OWNER" in flags
        assert "HIGH:TAKEBACK_OWNERSHIP" in flags

    @pytest.mark.asyncio
    async def test_analyze_flags_high_tax(self, provider):
        data = {"buy_tax": "0.12", "sell_tax": "0.20"}
        flags = provider._analyze_flags(data)
        assert any("HIGH_BUY_TAX" in f for f in flags)
        assert any("HIGH_SELL_TAX" in f for f in flags)

    @pytest.mark.asyncio
    async def test_analyze_flags_medium_tax(self, provider):
        data = {"buy_tax": "0.07", "sell_tax": "0.08"}
        flags = provider._analyze_flags(data)
        assert any("MEDIUM_BUY_TAX" in f for f in flags)
        assert any("MEDIUM_SELL_TAX" in f for f in flags)

    @pytest.mark.asyncio
    async def test_analyze_flags_low_holders(self, provider):
        data = {"holder_count": "10", "lp_holder_count": "1"}
        flags = provider._analyze_flags(data)
        assert "LOW_HOLDER_COUNT" in flags
        assert "HIGH:FEW_LP_HOLDERS" in flags

    @pytest.mark.asyncio
    async def test_risk_level_honeypot(self, provider):
        assert provider._determine_risk_level(["HONEYPOT"]) == "CRITICAL"

    @pytest.mark.asyncio
    async def test_risk_level_critical(self, provider):
        assert provider._determine_risk_level(["CRITICAL:MINTABLE"]) == "CRITICAL"

    @pytest.mark.asyncio
    async def test_risk_level_high(self, provider):
        assert provider._determine_risk_level(["HIGH:PROXY_CONTRACT"]) == "HIGH"

    @pytest.mark.asyncio
    async def test_risk_level_low(self, provider):
        assert provider._determine_risk_level([]) == "LOW"

    @pytest.mark.asyncio
    async def test_check_handles_exception(self, provider):
        with patch.object(provider, '_get_session', side_effect=Exception("timeout")):
            result = await provider.check("base", "0x123")
            assert result.risk_level == "UNKNOWN"
            assert "PROVIDER_ERROR" in result.flags
