# Contributing to Multi-Chain Memecoin Hunter

Thank you for your interest in contributing! This document provides guidelines and instructions for contributing to the project.

## Code of Conduct

- Be respectful and inclusive
- Focus on the code, not the person
- Help others learn and grow
- Report issues responsibly

## Getting Started

### Prerequisites

- Python 3.11+
- Git
- Redis 7.0+
- PostgreSQL 15+

### Setup Development Environment

```bash
# Clone the repository
git clone https://github.com/yourusername/multichain-memecoin-hunter.git
cd multichain-memecoin-hunter

# Create virtual environment
python3.11 -m venv venv
source venv/bin/activate  # On Windows: venv\Scripts\activate

# Install dependencies
pip install -r requirements.txt

# Install pre-commit hooks
pre-commit install
```

## Development Workflow

### 1. Create a Branch

```bash
# Create feature branch
git checkout -b feature/your-feature-name

# Or bugfix branch
git checkout -b bugfix/issue-description
```

### 2. Make Changes

- Write clean, readable code
- Follow PEP 8 style guide
- Add type hints
- Include docstrings
- Write tests for new functionality

### 3. Code Style

**Format code:**
```bash
black src/ tests/
isort src/ tests/
```

**Check style:**
```bash
flake8 src/ tests/
mypy src/
```

**Run tests:**
```bash
pytest tests/ -v --cov=src/
```

### 4. Commit Messages

Follow conventional commits:

```
feat: add new feature
fix: fix bug
docs: update documentation
test: add tests
refactor: refactor code
perf: improve performance
chore: update dependencies
```

Example:
```
feat: add Arbitrum adapter

- Implement ArbitrumAdapter class
- Add Camelot DEX support
- Add Flashbots MEV protection
- Add tests for Arbitrum adapter
```

### 5. Push and Create Pull Request

```bash
git push origin feature/your-feature-name
```

Then create a PR on GitHub with:
- Clear title and description
- Link to related issues
- Screenshots/examples if applicable
- Test results

## Testing

### Unit Tests

```bash
pytest tests/test_adapters.py -v
pytest tests/test_scoring.py -v
pytest tests/test_execution.py -v
```

### Integration Tests

```bash
pytest tests/integration/ -v
```

### Coverage

```bash
pytest --cov=src/ --cov-report=html
```

### Async Tests

```bash
pytest tests/test_async.py -v -s
```

## Documentation

### Adding Documentation

1. Update relevant `.md` files in `docs/`
2. Add docstrings to code
3. Update README if needed
4. Include examples

### Docstring Format

```python
def function_name(param1: str, param2: int) -> bool:
    """
    Brief description of what the function does.
    
    Longer description if needed, explaining the logic,
    edge cases, or important details.
    
    Args:
        param1: Description of param1
        param2: Description of param2
    
    Returns:
        Description of return value
    
    Raises:
        ValueError: When something is invalid
        RuntimeError: When something goes wrong
    
    Example:
        >>> result = function_name("test", 42)
        >>> print(result)
        True
    """
    pass
```

## Types of Contributions

### Bug Reports

1. Check if issue already exists
2. Provide minimal reproducible example
3. Include system information
4. Describe expected vs actual behavior

### Feature Requests

1. Describe the use case
2. Explain why it's needed
3. Provide examples
4. Discuss implementation approach

### Documentation

- Fix typos
- Clarify confusing sections
- Add examples
- Improve API documentation

### Code Improvements

- Refactoring
- Performance optimization
- Better error handling
- Test coverage

## Pull Request Process

1. **Before submitting:**
   - Run tests: `pytest`
   - Check style: `black`, `flake8`, `mypy`
   - Update documentation
   - Add changelog entry

2. **PR Description:**
   - What does this PR do?
   - Why is it needed?
   - How to test it?
   - Any breaking changes?

3. **Review process:**
   - Maintainers will review
   - May request changes
   - Be responsive to feedback
   - Address all comments

4. **Merge:**
   - Squash commits if needed
   - Update CHANGELOG
   - Close related issues

## Project Structure

```
src/
├── adapters/          # Chain adapters (Solana, Base, etc.)
├── services/          # Core services (Scoring, Execution, etc.)
├── models/            # Data models and types
└── main.py            # Entry point

tests/
├── test_adapters.py
├── test_scoring.py
├── test_execution.py
└── integration/       # Integration tests

docs/
├── 01-architecture-solana.md
├── 02-base-adapter.md
├── 03-scoring-execution.md
└── API_REFERENCE.md
```

## Coding Standards

### Python Style

- Use type hints
- Follow PEP 8
- Max line length: 100
- Use async/await for I/O

### Error Handling

```python
try:
    result = await api_call()
except SpecificError as e:
    logger.error(f"Specific error: {e}")
    raise
except Exception as e:
    logger.error(f"Unexpected error: {e}")
    raise
```

### Logging

```python
import logging

logger = logging.getLogger(__name__)

logger.debug("Debug message")
logger.info("Info message")
logger.warning("Warning message")
logger.error("Error message")
```

### Testing

```python
@pytest.mark.asyncio
async def test_adapter_initialization():
    """Test that adapter initializes correctly."""
    adapter = SolanaAdapter(config)
    assert adapter.chain == ChainId.SOLANA
    
    await adapter.start()
    assert adapter._ws is not None
    
    await adapter.stop()
```

## Security

### Reporting Security Issues

**DO NOT** open a public issue for security vulnerabilities.

Instead:
1. Email security@example.com
2. Include detailed description
3. Provide proof of concept if possible
4. Allow time for fix before disclosure

### Security Guidelines

- Never commit API keys or secrets
- Use environment variables
- Validate all inputs
- Sanitize error messages
- Use HTTPS for all connections
- Keep dependencies updated

## Performance Considerations

- Use async/await for I/O
- Cache when appropriate
- Minimize API calls
- Use connection pooling
- Monitor resource usage

## Release Process

1. Update version in `__init__.py`
2. Update CHANGELOG.md
3. Create release notes
4. Tag release: `git tag v1.0.0`
5. Push tag: `git push origin v1.0.0`

## Questions?

- Check existing issues/discussions
- Read documentation
- Ask in discussions
- Open an issue

## Recognition

Contributors will be recognized in:
- CONTRIBUTORS.md
- Release notes
- GitHub contributors page

Thank you for contributing! 🎉
