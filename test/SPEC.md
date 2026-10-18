# Trade Engine POC — Spec (C++17)

A simple, single-threaded limit order book matching engine for one instrument.

## Domain
- `Side { Buy, Sell }`, `OrderType { Limit, Market }`
- Prices are integer ticks (`int64_t`), quantities are `uint64_t`. No floating point.
- `Order { uint64_t id; Side side; OrderType type; int64_t price; uint64_t qty; }`
  (`price` ignored for Market orders)
- `Trade { uint64_t buy_id; uint64_t sell_id; int64_t price; uint64_t qty; }`

## Matching rules
1. Price-time priority: best price first; FIFO within a price level.
2. An incoming Buy limit matches resting asks with price <= its limit; Sell limit matches bids with price >= its limit.
3. Trade price = the resting order's price.
4. Partial fills allowed. Unfilled remainder of a Limit order rests on the book; unfilled remainder of a Market order is discarded (never rests).
5. Reject (return `Rejected`, no state change): qty == 0, duplicate live id, Limit with price <= 0.
6. `cancel(id)` removes a resting order in O(1) average; returns false if unknown/already filled.
7. Empty price levels are removed immediately.

## Clarifications (decided after spec review)
- C1. Market orders: `price` is never read. A Market buy/sell walks all opposite levels. On an empty opposite side: `Accepted`, no trades, `remaining == qty`.
- C2. "Live id" = currently resting on the book. Ids may be reused once the order is fully filled or cancelled. An incoming order (Limit or Market) whose id equals a resting order's id is rejected. Fully filled orders must not remain in the cancel index.
- C3. No self-trade prevention (no participant field; ids are unique among live orders).
- C4. On `Rejected`: `trades` empty, `remaining == o.qty`.
- C5. When crossing multiple levels, each trade uses its own resting level's price.
- C6. A partially filled resting order keeps its queue position. Cancel reduces level volume by the order's *current* remaining qty.
- C7. Equal price crosses: Buy limit P matches ask at P; Sell limit P matches bid at P.
- C8. Quantity bound: `kMaxQty = 1'000'000'000'000` (1e12, defined in types.hpp). Reject qty > kMaxQty. Fill qty = min(incoming remaining, resting remaining).
- C9. `order_count` counts resting orders only (never Market orders or fully filled orders).
- C10. Benchmark uses a fixed seed and prints orders/sec plus a checksum (sum of trade qty*price, and trade count).

## API (`include/engine/order_book.hpp`, namespace `engine`)
```cpp
enum class AddStatus { Accepted, Rejected };
struct AddResult { AddStatus status; std::vector<Trade> trades; uint64_t remaining; };

class OrderBook {
 public:
  AddResult add(const Order& o);
  bool cancel(uint64_t id);
  std::optional<int64_t> best_bid() const;
  std::optional<int64_t> best_ask() const;
  uint64_t volume_at(Side s, int64_t price) const; // total resting qty at level, 0 if none
  size_t order_count() const;                      // live resting orders
};
```
`remaining` = qty left after matching (resting for Limit, discarded for Market).

## Extension E1 (TASK-006, POC scope)
- E1.1 `struct Level { int64_t price; uint64_t volume; };` (in `types.hpp`).
- E1.2 `std::vector<Level> depth(Side s, size_t max_levels) const;` best price first
  (bids descending, asks ascending), at most `max_levels` entries, `max_levels == 0` → empty.
  `volume` = total resting qty at that level (same value `volume_at` would return).
- E1.3 `bool amend(uint64_t id, uint64_t new_qty);` reduce-only quantity amend.
  Returns false and changes nothing if id is not live, `new_qty == 0`, or `new_qty >= current remaining qty`.
  On success the order's remaining qty becomes `new_qty`, it **keeps its queue position**, and level volume
  drops by the difference. (Increases / price changes are done via cancel + add, which loses priority.)
- E1.4 `volume_at` must be O(log levels): keep a cached per-level total, updated on add, fill, cancel and amend.
  Observable behaviour of all existing APIs is unchanged; all existing tests must still pass.
  The internal per-level struct is named `PriceLevel` (`{ Queue orders; uint64_t volume; }`), not `Level`.
- E1.5 `depth` on an empty side returns an empty vector.
- E1.6 Aggregate bound (POC, not enforced): total resting qty at one price level is assumed to fit in
  `uint64_t` (≈1.8e7 orders at `kMaxQty`). Overflow beyond that is out of scope.

## Layout
```
test/
  CMakeLists.txt
  include/engine/types.hpp
  include/engine/order_book.hpp
  src/order_book.cpp
  src/main.cpp              # demo + simple throughput benchmark (1M random orders)
  tests/test_order_book.cpp # dependency-free assert-style tests, returns nonzero on failure
```
Suggested internals: `std::map` per side (bids descending, asks ascending) of `std::list<Order>`,
plus `std::unordered_map<uint64_t, Locator>` for O(1) cancel.
