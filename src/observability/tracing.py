"""Distributed Tracing.

trace_id and correlation_id through the entire flow.
Integrates with event system's correlation_id and causation_id.
"""

import logging
import time
import uuid
import json
from contextvars import ContextVar
from typing import Optional

logger = logging.getLogger(__name__)

# Context variables for trace propagation
_current_trace_id: ContextVar[str] = ContextVar('trace_id', default='')
_current_span_id: ContextVar[str] = ContextVar('span_id', default='')
_current_correlation_id: ContextVar[str] = ContextVar('correlation_id', default='')


def generate_trace_id() -> str:
    return uuid.uuid4().hex[:16]

def generate_span_id() -> str:
    return uuid.uuid4().hex[:8]


class Span:
    """A tracing span representing a unit of work."""

    def __init__(self, name: str, trace_id: str = "", parent_span_id: str = "",
                 correlation_id: str = ""):
        self.name = name
        self.trace_id = trace_id or _current_trace_id.get() or generate_trace_id()
        self.span_id = generate_span_id()
        self.parent_span_id = parent_span_id or _current_span_id.get()
        self.correlation_id = correlation_id or _current_correlation_id.get()
        self.start_time = time.time()
        self.end_time: Optional[float] = None
        self.tags: dict[str, str] = {}
        self.status: str = "OK"
        self.error: Optional[str] = None

    def set_tag(self, key: str, value: str):
        self.tags[key] = value
        return self

    def set_error(self, error: str):
        self.status = "ERROR"
        self.error = error
        return self

    def finish(self):
        self.end_time = time.time()
        duration_ms = (self.end_time - self.start_time) * 1000

        log_data = {
            "trace_id": self.trace_id,
            "span_id": self.span_id,
            "parent_span_id": self.parent_span_id,
            "correlation_id": self.correlation_id,
            "name": self.name,
            "duration_ms": round(duration_ms, 2),
            "status": self.status,
            "tags": self.tags,
        }
        if self.error:
            log_data["error"] = self.error

        logger.debug(f"span_complete: {json.dumps(log_data)}")
        return self

    @property
    def duration_ms(self) -> float:
        if self.end_time:
            return (self.end_time - self.start_time) * 1000
        return (time.time() - self.start_time) * 1000

    def __enter__(self):
        _current_trace_id.set(self.trace_id)
        _current_span_id.set(self.span_id)
        if self.correlation_id:
            _current_correlation_id.set(self.correlation_id)
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        if exc_type:
            self.set_error(str(exc_val))
        self.finish()
        return False


class Tracer:
    """Simple tracer for creating spans."""

    def __init__(self, service_name: str):
        self.service_name = service_name

    def start_span(self, name: str, **kwargs) -> Span:
        span = Span(name, **kwargs)
        span.set_tag("service", self.service_name)
        return span

    def get_current_trace_id(self) -> str:
        return _current_trace_id.get()

    def get_current_correlation_id(self) -> str:
        return _current_correlation_id.get()

    def set_correlation_id(self, correlation_id: str):
        _current_correlation_id.set(correlation_id)
