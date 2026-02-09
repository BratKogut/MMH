"""
MMH v3.1 -- Optional ZMQ fast-lane for low-latency local communication.

CRITICAL RULES:
    - This is NOT a durable transport.
    - Must NEVER be the only path for any event.
    - Redis Streams (:mod:`redis_streams`) is always the source of truth.
    - This is purely a cache / fast-lane for latency optimisation.
    - Controlled by the ``zmq_enabled`` feature flag in settings.

When enabled, the ZMQ fast-lane provides a PUB/SUB overlay that delivers
events to co-located consumers with sub-millisecond latency.  Every event
published here MUST also be published to Redis Streams independently.

If ``zmq`` (pyzmq) is not installed, or the feature flag is off, all
methods degrade gracefully to no-ops.
"""

from __future__ import annotations

import asyncio
import logging
from typing import Any, Callable, Optional

logger = logging.getLogger(__name__)

# Attempt to import zmq; degrade gracefully if not installed
try:
    import zmq
    import zmq.asyncio

    _ZMQ_AVAILABLE: bool = True
except ImportError:
    _ZMQ_AVAILABLE = False
    logger.info(
        "pyzmq not installed -- ZMQFastLane will be disabled. "
        "Install with: pip install pyzmq"
    )


class ZMQFastLane:
    """Optional ZMQ accelerator for local low-latency communication.

    CRITICAL RULES:
        - This is NOT a durable transport.
        - Must NEVER be the only path for any event.
        - Redis Streams is always the source of truth.
        - This is just a cache / fast-lane for latency optimisation.
        - Controlled by feature flag (``zmq_enabled`` in config).

    When ``enabled=False`` (or pyzmq is not installed), all methods
    are safe no-ops.

    Args:
        pub_address: ZMQ PUB socket bind address,
            e.g. ``"tcp://127.0.0.1:5555"``.
        sub_address: ZMQ SUB socket connect address,
            e.g. ``"tcp://127.0.0.1:5556"``.
        enabled: Feature flag.  If ``False``, the fast-lane is entirely
            inert.
    """

    def __init__(
        self,
        pub_address: str = "tcp://127.0.0.1:5555",
        sub_address: str = "tcp://127.0.0.1:5556",
        enabled: bool = False,
    ) -> None:
        self._enabled: bool = enabled and _ZMQ_AVAILABLE
        self._pub_address: str = pub_address
        self._sub_address: str = sub_address

        self._context: Any = None  # zmq.asyncio.Context when active
        self._pub_socket: Any = None  # zmq.asyncio.Socket (PUB)
        self._sub_socket: Any = None  # zmq.asyncio.Socket (SUB)

        self._handlers: dict[str, list[Callable[..., Any]]] = {}
        self._subscriber_task: Optional[asyncio.Task[None]] = None
        self._running: bool = False

        # Counters
        self.stats: dict[str, int] = {
            "published": 0,
            "received": 0,
            "errors": 0,
        }

    @property
    def is_enabled(self) -> bool:
        """Return True if the ZMQ fast-lane is enabled and available."""
        return self._enabled

    # -- lifecycle -----------------------------------------------------------

    async def start(self) -> None:
        """Start ZMQ PUB/SUB sockets if enabled.

        If the feature flag is off or pyzmq is not installed, this is a
        safe no-op.
        """
        if not self._enabled:
            logger.debug("ZMQFastLane is disabled -- start() is a no-op")
            return

        if not _ZMQ_AVAILABLE:
            logger.warning(
                "ZMQFastLane enabled in config but pyzmq is not installed. "
                "Disabling."
            )
            self._enabled = False
            return

        try:
            self._context = zmq.asyncio.Context()

            # PUB socket -- binds
            self._pub_socket = self._context.socket(zmq.PUB)
            self._pub_socket.setsockopt(zmq.SNDHWM, 10_000)
            self._pub_socket.setsockopt(zmq.LINGER, 0)
            self._pub_socket.bind(self._pub_address)

            # SUB socket -- connects
            self._sub_socket = self._context.socket(zmq.SUB)
            self._sub_socket.setsockopt(zmq.RCVHWM, 10_000)
            self._sub_socket.setsockopt(zmq.LINGER, 0)
            self._sub_socket.connect(self._sub_address)

            self._running = True

            # Start subscriber dispatch loop
            self._subscriber_task = asyncio.create_task(
                self._subscriber_loop(),
                name="zmq-subscriber-loop",
            )

            logger.info(
                "ZMQFastLane started: pub=%s sub=%s",
                self._pub_address,
                self._sub_address,
            )

        except Exception as exc:
            logger.error("ZMQFastLane start failed: %s", exc)
            self._enabled = False
            await self._cleanup_sockets()
            raise

    async def stop(self) -> None:
        """Stop ZMQ sockets and background tasks.

        Safe to call even if not started or already stopped.
        """
        if not self._running:
            return

        self._running = False

        # Cancel subscriber task
        if self._subscriber_task is not None and not self._subscriber_task.done():
            self._subscriber_task.cancel()
            try:
                await self._subscriber_task
            except asyncio.CancelledError:
                pass
            self._subscriber_task = None

        await self._cleanup_sockets()

        logger.info("ZMQFastLane stopped. stats=%s", self.stats)

    async def _cleanup_sockets(self) -> None:
        """Close ZMQ sockets and destroy context."""
        try:
            if self._pub_socket is not None:
                self._pub_socket.close(linger=0)
                self._pub_socket = None
            if self._sub_socket is not None:
                self._sub_socket.close(linger=0)
                self._sub_socket = None
            if self._context is not None:
                self._context.term()
                self._context = None
        except Exception as exc:
            logger.warning("Error cleaning up ZMQ sockets: %s", exc)

    # -- publish -------------------------------------------------------------

    async def publish(self, topic: str, data: bytes) -> None:
        """Publish data to a ZMQ topic (fire-and-forget, no durability).

        The message is a two-frame multipart: ``[topic_bytes, data]``.

        REMINDER: This does NOT replace Redis Streams.  Every event
        published here MUST also be published to the durable bus.

        Args:
            topic: Topic string (e.g. ``"tokens:new:solana"``).
            data: Raw bytes payload.
        """
        if not self._enabled or self._pub_socket is None:
            return

        try:
            topic_bytes: bytes = topic.encode("utf-8")
            await self._pub_socket.send_multipart([topic_bytes, data])
            self.stats["published"] += 1
            logger.debug(
                "ZMQ published to topic '%s' (%d bytes)", topic, len(data)
            )
        except Exception as exc:
            self.stats["errors"] += 1
            logger.warning(
                "ZMQ publish to '%s' failed (non-fatal): %s", topic, exc
            )

    # -- subscribe -----------------------------------------------------------

    async def subscribe(
        self,
        topic: str,
        handler: Callable[..., Any],
    ) -> None:
        """Subscribe to a ZMQ topic.

        The *handler* receives the raw ``bytes`` payload (the second frame
        of the multipart message).  It may be sync or async.

        Multiple handlers can be registered for the same topic.

        Args:
            topic: Topic string to subscribe to.
            handler: Callable that accepts ``bytes`` payload.
        """
        if not self._enabled:
            logger.debug(
                "ZMQFastLane disabled -- subscribe('%s') is a no-op", topic
            )
            return

        if topic not in self._handlers:
            self._handlers[topic] = []
            # Subscribe on the socket
            if self._sub_socket is not None:
                topic_bytes: bytes = topic.encode("utf-8")
                self._sub_socket.setsockopt(zmq.SUBSCRIBE, topic_bytes)
                logger.debug("ZMQ subscribed to topic '%s'", topic)

        self._handlers[topic].append(handler)
        logger.info(
            "ZMQ handler registered for topic '%s' (total=%d)",
            topic,
            len(self._handlers[topic]),
        )

    # -- internal subscriber loop -------------------------------------------

    async def _subscriber_loop(self) -> None:
        """Background loop that receives messages and dispatches to handlers."""
        while self._running:
            try:
                if self._sub_socket is None:
                    await asyncio.sleep(0.1)
                    continue

                # Use poll to avoid blocking forever (allows checking _running)
                events: int = await self._sub_socket.poll(
                    timeout=1000, flags=zmq.POLLIN
                )
                if not events:
                    continue

                multipart: list[bytes] = await self._sub_socket.recv_multipart()
                if len(multipart) < 2:
                    logger.warning(
                        "ZMQ received malformed message (frames=%d)",
                        len(multipart),
                    )
                    continue

                topic_bytes: bytes = multipart[0]
                payload: bytes = multipart[1]
                topic: str = topic_bytes.decode("utf-8", errors="replace")

                self.stats["received"] += 1

                # Dispatch to registered handlers
                handlers: list[Callable[..., Any]] = self._handlers.get(topic, [])
                for handler in handlers:
                    try:
                        result = handler(payload)
                        # If handler is async, await it
                        if asyncio.iscoroutine(result):
                            await result
                    except Exception as handler_exc:
                        logger.warning(
                            "ZMQ handler error on topic '%s': %s",
                            topic,
                            handler_exc,
                        )
                        self.stats["errors"] += 1

            except asyncio.CancelledError:
                break
            except Exception as exc:
                logger.error("ZMQ subscriber loop error: %s", exc)
                self.stats["errors"] += 1
                await asyncio.sleep(0.5)

        logger.debug("ZMQ subscriber loop exited")
