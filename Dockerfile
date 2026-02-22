# =============================================================================
# MMH v3.1 — Multi-stage production Dockerfile
# =============================================================================

# --- Stage 1: Builder --------------------------------------------------------
FROM python:3.11-slim AS builder

WORKDIR /build

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    libpq-dev \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir --prefix=/install -r requirements.txt

# --- Stage 2: Runtime --------------------------------------------------------
FROM python:3.11-slim

# Create non-root user
RUN groupadd -r mmh && useradd -r -g mmh -s /bin/false mmh

WORKDIR /app

# Install only runtime dependencies (no build-essential)
RUN apt-get update && apt-get install -y --no-install-recommends \
    libpq5 \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Copy installed Python packages from builder
COPY --from=builder /install /usr/local

# Copy application code with correct ownership
COPY --chown=mmh:mmh src/ /app/src/
COPY --chown=mmh:mmh config/ /app/config/

# Create data directories with correct ownership
RUN mkdir -p /app/data/wal/solana /app/data/wal/base /app/data/snapshots \
    && chown -R mmh:mmh /app/data

# Set environment
ENV PYTHONPATH=/app
ENV PYTHONUNBUFFERED=1

# Expose Prometheus metrics port
EXPOSE 8000

# Switch to non-root user
USER mmh

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=15s --retries=3 \
    CMD curl -sf http://localhost:8000/ || exit 1

# Run the application
CMD ["python", "-m", "src.main"]
