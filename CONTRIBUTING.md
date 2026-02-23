# Contributing to NEXUS Memecoin Hunter

## Code of Conduct

- Be respectful and inclusive
- Focus on the code, not the person
- Help others learn and grow

## Getting Started

### Prerequisites

- Go 1.24+
- Git

### Setup Development Environment

```bash
# Clone the repository
git clone https://github.com/BratKogut/MMH.git
cd MMH/nexus

# Build all binaries
go build -o bin/ ./cmd/...

# Run tests
go test -race -count=1 ./...
```

## Development Workflow

### 1. Create a Branch

```bash
git checkout -b feature/your-feature-name
# Or bugfix branch
git checkout -b bugfix/issue-description
```

### 2. Make Changes

- Write clean, readable Go code
- Follow Go conventions (gofmt, golint)
- Write tests for new functionality
- Keep functions focused and small

### 3. Code Style

**Format code:**
```bash
gofmt -w .
```

**Run linter:**
```bash
golangci-lint run ./...
```

**Run tests:**
```bash
go test -race -count=1 ./...
```

**Run tests with coverage:**
```bash
go test -race -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
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
feat: add holder exodus detection to CSM

- Monitor top-10 holders for mass selling
- Trigger panic sell on exodus > 30% threshold
- Add tests for holder exodus detection
```

### 5. Push and Create Pull Request

```bash
git push origin feature/your-feature-name
```

Create a PR with:
- Clear title and description
- Link to related issues
- Test results

## Project Structure

```
nexus/
├── cmd/                    # Entry points (nexus-hunter, nexus-core, nexus-intel)
├── internal/
│   ├── scanner/            # Token Scanner + Analyzer + 5D Scoring + SellSim + Adaptive
│   ├── sniper/             # Sniper Engine + CSM + Exit Engine
│   ├── graph/              # Entity Graph Engine
│   ├── liquidity/          # Liquidity Flow Direction Analysis
│   ├── narrative/          # Narrative Momentum Engine
│   ├── honeypot/           # Honeypot Evolution Tracker
│   ├── correlation/        # Cross-Token Correlation Detector
│   ├── copytrade/          # Copy-Trade Intelligence
│   ├── solana/             # RPC + WebSocket + Jito + priority fees
│   ├── adapters/           # Exchange adapters (Jupiter, Kraken, Binance)
│   ├── bus/                # Kafka/RedPanda event streaming
│   ├── clickhouse/         # Analytics writer
│   ├── execution/          # Order engine + state machine
│   ├── risk/               # Risk engine
│   ├── intel/              # LLM intelligence pipeline
│   ├── config/             # YAML configuration
│   └── observability/      # Prometheus + health
└── docs/                   # Documentation
```

## Testing

### Unit Tests

```bash
# Run all tests
go test -race -count=1 ./...

# Run specific package tests
go test -race ./internal/scanner/...
go test -race ./internal/sniper/...
go test -race ./internal/graph/...
```

### Test Coverage

```bash
go test -race -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

**Current status:** 497 tests passing, ~34,000 LOC

## Coding Standards

### Go Style

- Use `gofmt` for formatting
- Follow [Effective Go](https://go.dev/doc/effective_go)
- Use meaningful variable names
- Keep functions under 50 lines where possible
- Use interfaces for testability

### Error Handling

```go
result, err := someFunction()
if err != nil {
    return fmt.Errorf("context: %w", err)
}
```

### Logging

```go
log.Printf("[MODULE] Action: %s result=%v", param, result)
```

### Testing

```go
func TestFeature_Scenario(t *testing.T) {
    // Arrange
    input := setupTestData()

    // Act
    result := functionUnderTest(input)

    // Assert
    if result != expected {
        t.Errorf("expected %v, got %v", expected, result)
    }
}
```

## Types of Contributions

### Bug Reports
1. Check if issue already exists
2. Provide minimal reproducible example
3. Include Go version and OS
4. Describe expected vs actual behavior

### Feature Requests
1. Describe the use case
2. Explain why it's needed
3. Discuss implementation approach

### Code Improvements
- Bug fixes
- Performance optimization
- Test coverage
- Documentation

## Security

### Reporting Security Issues

Do NOT open a public issue for security vulnerabilities. Report privately.

### Security Guidelines

- Never commit API keys or private keys
- Use YAML config files with .gitignore
- Validate all external inputs
- Use HTTPS for all connections

## Pull Request Process

1. Run tests: `go test -race ./...`
2. Format code: `gofmt -w .`
3. Update documentation if needed
4. Create PR with clear description
5. Address review feedback

---

*Last Updated: 2026-02-23*
