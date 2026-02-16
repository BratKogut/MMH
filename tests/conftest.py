"""Pytest configuration and shared fixtures."""
import pytest
import asyncio
import sys
import os

# Ensure the project root is on sys.path so 'src' is importable
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))


@pytest.fixture(scope="session")
def event_loop():
    """Create event loop for async tests."""
    loop = asyncio.new_event_loop()
    yield loop
    loop.close()
