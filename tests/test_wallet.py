"""Tests for wallet manager module."""

import pytest
from unittest.mock import MagicMock


class TestWalletManager:
    """Tests for WalletManager."""

    def _make_settings(self, **kwargs):
        """Create mock settings."""
        defaults = {
            "solana_private_key": "",
            "evm_private_key": "",
            "solana_rpc_url": "",
            "base_rpc_url": "",
        }
        defaults.update(kwargs)
        settings = MagicMock()
        for k, v in defaults.items():
            setattr(settings, k, v)
        return settings

    def test_not_configured_when_no_keys(self):
        from src.wallet.wallet_manager import WalletManager
        settings = self._make_settings()
        wm = WalletManager(settings)
        assert wm.is_configured("solana") is False
        assert wm.is_configured("base") is False

    def test_get_address_no_wallet(self):
        from src.wallet.wallet_manager import WalletManager
        settings = self._make_settings()
        wm = WalletManager(settings)
        assert wm.get_address("solana") is None
        assert wm.get_address("base") is None

    @pytest.mark.asyncio
    async def test_get_wallet_no_config(self):
        from src.wallet.wallet_manager import WalletManager
        settings = self._make_settings()
        wm = WalletManager(settings)
        result = await wm.get_wallet("solana")
        assert result is None

    @pytest.mark.asyncio
    async def test_has_sufficient_balance_no_wallet(self):
        from src.wallet.wallet_manager import WalletManager
        settings = self._make_settings()
        wm = WalletManager(settings)
        assert await wm.has_sufficient_balance("solana", 10.0) is False
