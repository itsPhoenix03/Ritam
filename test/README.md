# Trade Engine POC

A dependency-free C++17, single-threaded limit order book for one instrument.
It matches limit and market orders using price-time priority and trades at the
resting order's price. Unfilled limit quantities rest; market remainders are
discarded. Prices are integer ticks and quantities are unsigned integers.

## API

`engine::OrderBook` is declared in `include/engine/order_book.hpp`:

| Method | Behavior |
| --- | --- |
| `add(order)` | Returns status, trades, and remaining quantity. Rejects zero/oversized quantity, nonpositive limit prices, and duplicate live IDs without changing state. |
| `cancel(id)` | Removes a resting order; returns false for unknown or filled IDs. |
| `amend(id, new_qty)` | Reduces current remaining quantity while preserving queue position. Returns false without changes for unknown IDs, zero, equal quantity, or increases. |
| `best_bid()` / `best_ask()` | Returns an optional best price. |
| `volume_at(side, price)` | Returns cached total resting quantity at a price, or zero; O(log levels). |
| `depth(side, max_levels)` | Returns up to the requested number of `Level { price, volume }` snapshots, best price first. Zero limit or an empty side returns an empty vector. |
| `order_count()` | Returns the number of resting orders. |

Individual quantities must be at most `kMaxQty` (1e12). Aggregate quantity at each
price level is assumed to fit in `uint64_t`; this POC does not enforce that bound.
See [SPEC.md](SPEC.md) for the complete contract and E1 extension.

## Build and test

Requires CMake 3.14 or newer and a C++17 compiler. From the repository root:

```sh
cmake -S test -B build && cmake --build build --config Release && ctest --test-dir build -C Release
```

For shells without `&&`, run the three commands separately, continuing only when
each succeeds. Tests use dependency-free checks and a fixed-seed differential
comparison against a simple reference book.

Run `build/trade_engine` (single-configuration builds) or
`build/Release/trade_engine.exe` (Visual Studio builds) for the matching, depth,
and quantity-amend demo followed by a fixed-seed benchmark of one million orders.
The benchmark prints throughput, trade count, and a quantity-times-price checksum.
