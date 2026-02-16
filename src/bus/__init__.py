"""
MMH v3.1 -- Event Bus package.

The event bus is the nervous system of the MMH trading pipeline.
Redis Streams is the SINGLE durable transport.  ZMQ is an optional
low-latency accelerator (feature flag only, never sole path).

Quick start::

    from src.bus import RedisEventBus, StreamNames, ReliableConsumer

    bus = RedisEventBus("redis://localhost:6379")
    await bus.connect()

    # Publish
    stream = StreamNames.for_chain(StreamNames.TOKENS_NEW, "solana")
    await bus.publish(stream, {"mint": "abc", "event_id": "..."})

    # Consume (reliable)
    consumer = ReliableConsumer(
        bus=bus,
        stream=stream,
        group="sanitizer",
        consumer_name="sanitizer-1",
        handler=my_handler,
    )
    await consumer.start()
"""

from src.bus.consumer import RedisIdempotencyStore, ReliableConsumer
from src.bus.dlq import DeadLetterQueue
from src.bus.redis_streams import RedisEventBus, StreamNames
from src.bus.zmq_fastlane import ZMQFastLane

__all__: list[str] = [
    "RedisEventBus",
    "StreamNames",
    "ReliableConsumer",
    "RedisIdempotencyStore",
    "DeadLetterQueue",
    "ZMQFastLane",
]
