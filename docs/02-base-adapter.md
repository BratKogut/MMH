# Base Chain Adapter — Legacy Specification

> **Note:** This document is a legacy specification from the v2.0 multi-chain design.
> The Base chain adapter has NOT been implemented in the current Go v3.2 codebase.
> The project currently focuses exclusively on Solana.
>
> This spec is retained for reference if Base chain support is added in the future.
> See [ROADMAP.md](../ROADMAP.md) for the current development plan.

---

## Overview

The Base adapter was planned to monitor Uniswap V3 and Aerodrome pool creation events
on Base L2, perform security checks via GoPlus API, and execute swaps via the 0x aggregator.

## Planned Features

- WebSocket monitoring of Uniswap V3 and Aerodrome PoolCreated events
- GoPlus security API integration (honeypot, mintable, tax detection)
- Basescan contract verification
- 0x aggregator for swap execution
- Uniswap V3 exactInputSingle for direct swaps

## Key Addresses (Base)

| Contract | Address |
|----------|---------|
| Uniswap V3 Factory | 0x33128a8fC17869897dcE68Ed026d694621f6FDfD |
| Uniswap V3 Router | 0xE592427A0AEce92De3Edee1F18E0157C05861564 |
| Aerodrome Factory | 0x420DD381b31aEf6683db6B902084cB0FFECe40Da |
| WETH (Base) | 0x4200000000000000000000000000000000000006 |
| USDC (Base) | 0x833589fcd6edb6e08f4c7c32d4f71b54bda02913 |

## Implementation Status

**NOT IMPLEMENTED** — Solana-first strategy. Base support is planned for after
the Paper Marathon (S7) and Live Trading (S8) sprints prove profitability on Solana.

---

*Last Updated: 2026-02-23*
