// Black-box tests for engine::OrderBook, written from SPEC.md and its clarifications only.
// Dependency-free: CHECK / CHECK_EQ count failures; main returns nonzero on any failure.

#include "engine/order_book.hpp"
#include "engine/types.hpp"

#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <limits>
#include <optional>
#include <random>
#include <string>
#include <utility>
#include <vector>

using namespace engine;

namespace {

int g_failures = 0;
int g_checks = 0;

const std::int64_t kMaxPx = std::numeric_limits<std::int64_t>::max();
const std::uint64_t kMaxU64 = std::numeric_limits<std::uint64_t>::max();
// C8: the largest valid quantity. The spec fixes the value at 1e12.
static_assert(kMaxQty == 1'000'000'000'000ULL, "kMaxQty must match SPEC.md C8");

#define CHECK(cond)                                                                 \
  do {                                                                              \
    ++g_checks;                                                                     \
    if (!(cond)) {                                                                  \
      ++g_failures;                                                                 \
      std::fprintf(stderr, "%s:%d: CHECK(%s) failed\n", __FILE__, __LINE__, #cond); \
    }                                                                               \
  } while (0)

#define CHECK_EQ(a, b)                                                             \
  do {                                                                             \
    ++g_checks;                                                                    \
    const auto lhs_ = (a);                                                         \
    const auto rhs_ = (b);                                                         \
    if (!(lhs_ == rhs_)) {                                                         \
      ++g_failures;                                                                \
      std::fprintf(stderr, "%s:%d: CHECK_EQ(%s, %s) failed: %s != %s\n",           \
                   __FILE__, __LINE__, #a, #b,                                     \
                   std::to_string(lhs_).c_str(), std::to_string(rhs_).c_str());    \
    }                                                                              \
  } while (0)

Order make(std::uint64_t id, Side side, OrderType type, std::int64_t price, std::uint64_t qty) {
  Order o;
  o.id = id;
  o.side = side;
  o.type = type;
  o.price = price;
  o.qty = qty;
  return o;
}

Order lim(std::uint64_t id, Side side, std::int64_t price, std::uint64_t qty) {
  return make(id, side, OrderType::Limit, price, qty);
}

Order mkt(std::uint64_t id, Side side, std::uint64_t qty) {
  return make(id, side, OrderType::Market, 0, qty);
}

Trade trd(std::uint64_t buy_id, std::uint64_t sell_id, std::int64_t price, std::uint64_t qty) {
  Trade t;
  t.buy_id = buy_id;
  t.sell_id = sell_id;
  t.price = price;
  t.qty = qty;
  return t;
}

void expect_trades(const AddResult& r, const std::vector<Trade>& want) {
  CHECK_EQ(r.trades.size(), want.size());
  const std::size_t n = r.trades.size() < want.size() ? r.trades.size() : want.size();
  for (std::size_t i = 0; i < n; ++i) {
    CHECK_EQ(r.trades[i].buy_id, want[i].buy_id);
    CHECK_EQ(r.trades[i].sell_id, want[i].sell_id);
    CHECK_EQ(r.trades[i].price, want[i].price);
    CHECK_EQ(r.trades[i].qty, want[i].qty);
  }
}

// C4: a rejected add changes nothing, returns no trades, and reports remaining == qty.
void expect_rejected(const AddResult& r, const Order& o) {
  CHECK(r.status == AddStatus::Rejected);
  CHECK(r.trades.empty());
  CHECK_EQ(r.remaining, o.qty);
}

Level lvl(std::int64_t price, std::uint64_t volume) {
  Level l;
  l.price = price;
  l.volume = volume;
  return l;
}

// E1.2: depth levels compared in order (best price first).
void expect_levels(const std::vector<Level>& got, const std::vector<Level>& want) {
  CHECK_EQ(got.size(), want.size());
  const std::size_t n = got.size() < want.size() ? got.size() : want.size();
  for (std::size_t i = 0; i < n; ++i) {
    CHECK_EQ(got[i].price, want[i].price);
    CHECK_EQ(got[i].volume, want[i].volume);
  }
}

// Whole-book snapshot: both sides via depth (all levels) plus the live order count.
void expect_book(const OrderBook& ob, const std::vector<Level>& bids,
                 const std::vector<Level>& asks, std::size_t orders) {
  expect_levels(ob.depth(Side::Buy, 100), bids);
  expect_levels(ob.depth(Side::Sell, 100), asks);
  CHECK_EQ(ob.order_count(), orders);
}

// ---- Price-time priority ----

void test_fifo_within_level() {
  OrderBook ob;
  CHECK(ob.add(lim(1, Side::Buy, 100, 5)).status == AddStatus::Accepted);
  CHECK(ob.add(lim(2, Side::Buy, 100, 5)).status == AddStatus::Accepted);
  CHECK(ob.add(lim(3, Side::Buy, 100, 5)).status == AddStatus::Accepted);

  AddResult r = ob.add(mkt(10, Side::Sell, 7));
  CHECK(r.status == AddStatus::Accepted);
  expect_trades(r, {trd(1, 10, 100, 5), trd(2, 10, 100, 2)});
  CHECK_EQ(r.remaining, 0u);
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 8u);  // id2 has 3 left, id3 has 5
  CHECK_EQ(ob.order_count(), 2u);

  // C6: the partially filled id2 keeps its place at the front of the queue.
  r = ob.add(lim(11, Side::Sell, 100, 3));
  expect_trades(r, {trd(2, 11, 100, 3)});
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 5u);
  CHECK_EQ(ob.order_count(), 1u);
  CHECK(ob.best_bid() == 100);
}

void test_cancel_from_middle_keeps_fifo() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, 1));
  ob.add(lim(2, Side::Buy, 100, 1));
  ob.add(lim(3, Side::Buy, 100, 1));
  CHECK(ob.cancel(2));

  AddResult r = ob.add(mkt(10, Side::Sell, 2));
  expect_trades(r, {trd(1, 10, 100, 1), trd(3, 10, 100, 1)});
  CHECK_EQ(ob.order_count(), 0u);
  CHECK(!ob.best_bid());
}

void test_trade_price_is_resting_price() {
  OrderBook ob;
  // Resting ask at 100, incoming buy limit at 105 -> trades at 100.
  ob.add(lim(1, Side::Sell, 100, 5));
  AddResult r = ob.add(lim(2, Side::Buy, 105, 5));
  expect_trades(r, {trd(2, 1, 100, 5)});

  // Resting ask at 150, incoming buy limit at 160 -> trades at 150.
  ob.add(lim(4, Side::Sell, 150, 5));
  r = ob.add(lim(5, Side::Buy, 160, 5));
  expect_trades(r, {trd(5, 4, 150, 5)});

  // Resting bid at 100, incoming sell limit at 90 -> trades at 100.
  ob.add(lim(6, Side::Buy, 100, 5));
  r = ob.add(lim(7, Side::Sell, 90, 5));
  expect_trades(r, {trd(6, 7, 100, 5)});
  CHECK_EQ(ob.order_count(), 0u);
}

// ---- Partial fills ----

void test_partial_fill_incoming_buy_remainder_rests() {
  OrderBook ob;
  ob.add(lim(1, Side::Sell, 100, 10));

  AddResult r = ob.add(lim(2, Side::Buy, 100, 4));
  expect_trades(r, {trd(2, 1, 100, 4)});
  CHECK_EQ(r.remaining, 0u);
  CHECK_EQ(ob.volume_at(Side::Sell, 100), 6u);
  CHECK(ob.best_ask() == 100);
  CHECK(!ob.best_bid());

  r = ob.add(lim(3, Side::Buy, 100, 10));
  expect_trades(r, {trd(3, 1, 100, 6)});
  CHECK_EQ(r.remaining, 4u);  // unfilled part of a Limit rests
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 4u);
  CHECK(ob.best_bid() == 100);
  CHECK(!ob.best_ask());
  CHECK_EQ(ob.order_count(), 1u);
}

void test_partial_fill_incoming_sell_remainder_rests() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, 10));

  AddResult r = ob.add(lim(2, Side::Sell, 100, 15));
  expect_trades(r, {trd(1, 2, 100, 10)});
  CHECK_EQ(r.remaining, 5u);
  CHECK(ob.best_ask() == 100);
  CHECK_EQ(ob.volume_at(Side::Sell, 100), 5u);
  CHECK(!ob.best_bid());
  CHECK_EQ(ob.order_count(), 1u);
}

// ---- Sweeps, market remainders, limit cutoffs ----

void test_sweep_multiple_levels() {
  OrderBook ob;
  ob.add(lim(1, Side::Sell, 100, 5));
  ob.add(lim(2, Side::Sell, 101, 5));
  ob.add(lim(3, Side::Sell, 102, 5));

  // C5: each trade uses its own resting level's price.
  AddResult r = ob.add(lim(4, Side::Buy, 102, 12));
  expect_trades(r, {trd(4, 1, 100, 5), trd(4, 2, 101, 5), trd(4, 3, 102, 2)});
  CHECK_EQ(r.remaining, 0u);
  CHECK(ob.best_ask() == 102);
  CHECK_EQ(ob.volume_at(Side::Sell, 100), 0u);
  CHECK_EQ(ob.volume_at(Side::Sell, 101), 0u);
  CHECK_EQ(ob.volume_at(Side::Sell, 102), 3u);
  CHECK_EQ(ob.order_count(), 1u);

  // Limit at 101 must not cross the ask at 102; the remainder rests as a bid.
  r = ob.add(lim(5, Side::Buy, 101, 100));
  CHECK(r.trades.empty());
  CHECK_EQ(r.remaining, 100u);
  CHECK(ob.best_bid() == 101);
  CHECK(ob.best_ask() == 102);
}

void test_market_remainder_discarded() {
  OrderBook ob;
  ob.add(lim(1, Side::Sell, 100, 5));
  ob.add(lim(2, Side::Sell, 101, 5));

  AddResult r = ob.add(mkt(3, Side::Buy, 20));
  CHECK(r.status == AddStatus::Accepted);
  expect_trades(r, {trd(3, 1, 100, 5), trd(3, 2, 101, 5)});
  CHECK_EQ(r.remaining, 10u);
  CHECK(!ob.best_bid());  // market remainder never rests
  CHECK(!ob.best_ask());
  CHECK_EQ(ob.order_count(), 0u);

  // C1: market order against an empty side: accepted, no trades, nothing rests.
  r = ob.add(mkt(4, Side::Sell, 7));
  CHECK(r.status == AddStatus::Accepted);
  CHECK(r.trades.empty());
  CHECK_EQ(r.remaining, 7u);
  CHECK_EQ(ob.order_count(), 0u);
  CHECK(!ob.best_ask());
}

void test_market_price_is_ignored() {
  OrderBook ob;
  ob.add(lim(1, Side::Sell, 100, 3));
  AddResult r = ob.add(make(2, Side::Buy, OrderType::Market, -5, 3));
  CHECK(r.status == AddStatus::Accepted);
  expect_trades(r, {trd(2, 1, 100, 3)});
  CHECK_EQ(r.remaining, 0u);
}

// ---- Rejects ----

void test_reject_zero_qty() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, 5));

  Order a = lim(2, Side::Buy, 100, 0);
  expect_rejected(ob.add(a), a);
  Order b = lim(3, Side::Sell, 100, 0);
  expect_rejected(ob.add(b), b);
  Order c = mkt(4, Side::Sell, 0);
  expect_rejected(ob.add(c), c);

  // No state change.
  CHECK_EQ(ob.order_count(), 1u);
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 5u);
  CHECK(!ob.best_ask());
}

void test_reject_non_positive_limit_price() {
  OrderBook ob;
  const std::int64_t kMinPx = std::numeric_limits<std::int64_t>::min();
  Order a = lim(1, Side::Buy, 0, 5);
  Order b = lim(2, Side::Sell, 0, 5);
  Order c = lim(3, Side::Buy, -1, 5);
  Order d = lim(4, Side::Sell, -1, 5);
  Order e = lim(5, Side::Buy, kMinPx, 5);
  expect_rejected(ob.add(a), a);
  expect_rejected(ob.add(b), b);
  expect_rejected(ob.add(c), c);
  expect_rejected(ob.add(d), d);
  expect_rejected(ob.add(e), e);
  CHECK_EQ(ob.order_count(), 0u);
  CHECK(!ob.best_bid());
  CHECK(!ob.best_ask());
}

void test_reject_duplicate_live_id() {
  OrderBook ob;
  ob.add(lim(1, Side::Sell, 100, 5));

  // Duplicate limit id that would otherwise cross: rejected, no trade.
  Order dup = lim(1, Side::Buy, 100, 5);
  expect_rejected(ob.add(dup), dup);
  CHECK_EQ(ob.volume_at(Side::Sell, 100), 5u);
  CHECK_EQ(ob.order_count(), 1u);
  CHECK(!ob.best_bid());

  // C2: a market order reusing a live id is also rejected, and nothing trades.
  Order mdup = mkt(1, Side::Buy, 3);
  expect_rejected(ob.add(mdup), mdup);
  CHECK_EQ(ob.volume_at(Side::Sell, 100), 5u);
  CHECK_EQ(ob.order_count(), 1u);

  // The original order is intact and still cancellable.
  CHECK(ob.cancel(1));
  CHECK_EQ(ob.order_count(), 0u);
}

// ---- Id reuse and cancel semantics ----

void test_reuse_id_after_fill() {
  OrderBook ob;
  ob.add(lim(7, Side::Buy, 100, 5));
  AddResult r = ob.add(lim(8, Side::Sell, 100, 5));
  expect_trades(r, {trd(7, 8, 100, 5)});
  CHECK_EQ(ob.order_count(), 0u);

  r = ob.add(lim(7, Side::Buy, 99, 3));
  CHECK(r.status == AddStatus::Accepted);
  CHECK(ob.best_bid() == 99);
  CHECK_EQ(ob.order_count(), 1u);
  CHECK(ob.cancel(7));
}

void test_reuse_id_after_cancel() {
  OrderBook ob;
  ob.add(lim(9, Side::Sell, 110, 4));
  CHECK(ob.cancel(9));
  CHECK_EQ(ob.volume_at(Side::Sell, 110), 0u);
  CHECK(!ob.best_ask());

  CHECK(ob.add(lim(9, Side::Buy, 90, 4)).status == AddStatus::Accepted);
  CHECK(ob.best_bid() == 90);
  CHECK(!ob.best_ask());

  // The new id 9 is the live order: a market sell fills it.
  AddResult r = ob.add(mkt(20, Side::Sell, 4));
  expect_trades(r, {trd(9, 20, 90, 4)});
}

void test_cancel_unknown_and_filled() {
  OrderBook ob;
  CHECK(!ob.cancel(12345));  // empty book

  ob.add(lim(1, Side::Buy, 100, 5));
  CHECK(!ob.cancel(12345));  // unknown id, non-empty book
  CHECK_EQ(ob.order_count(), 1u);

  ob.add(mkt(2, Side::Sell, 5));  // fully fills id1
  CHECK(!ob.cancel(1));           // already filled
  CHECK_EQ(ob.order_count(), 0u);
  CHECK(!ob.best_bid());
}

void test_cancel_twice_and_partially_filled() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, 10));
  ob.add(lim(2, Side::Sell, 100, 4));  // partially fills id1 to 6 left
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 6u);

  // C6: cancel removes the order's current remaining qty from the level.
  CHECK(ob.cancel(1));
  CHECK(!ob.cancel(1));  // second cancel fails
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 0u);
  CHECK_EQ(ob.order_count(), 0u);
  CHECK(!ob.best_bid());
}

void test_empty_levels_removed() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, 5));
  ob.add(lim(2, Side::Buy, 99, 5));
  CHECK(ob.best_bid() == 100);

  CHECK(ob.cancel(1));
  CHECK(ob.best_bid() == 99);  // emptied level removed immediately
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 0u);

  ob.add(mkt(3, Side::Sell, 5));  // consumes the 99 level
  CHECK(!ob.best_bid());
  CHECK_EQ(ob.volume_at(Side::Buy, 99), 0u);
  CHECK_EQ(ob.order_count(), 0u);

  ob.add(lim(4, Side::Sell, 110, 5));
  CHECK(ob.cancel(4));
  CHECK(!ob.best_ask());
}

// ---- volume_at / order_count ----

void test_volume_at_and_order_count() {
  OrderBook ob;
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 0u);
  CHECK_EQ(ob.order_count(), 0u);

  ob.add(lim(1, Side::Sell, 100, 7));
  ob.add(lim(2, Side::Buy, 99, 5));  // no cross
  CHECK_EQ(ob.volume_at(Side::Sell, 100), 7u);
  CHECK_EQ(ob.volume_at(Side::Buy, 99), 5u);
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 0u);   // wrong side
  CHECK_EQ(ob.volume_at(Side::Sell, 99), 0u);   // wrong side
  CHECK_EQ(ob.volume_at(Side::Sell, 101), 0u);  // no level
  CHECK_EQ(ob.order_count(), 2u);

  ob.add(lim(3, Side::Buy, 95, 2));
  ob.add(lim(4, Side::Buy, 95, 3));
  CHECK_EQ(ob.volume_at(Side::Buy, 95), 5u);
  CHECK_EQ(ob.order_count(), 4u);
}

// ---- E1.1 / E1.2: depth ----

void test_depth_empty_book() {
  OrderBook ob;
  CHECK(ob.depth(Side::Buy, 5).empty());
  CHECK(ob.depth(Side::Sell, 5).empty());
  CHECK(ob.depth(Side::Buy, 0).empty());
  expect_book(ob, {}, {}, 0);
}

void test_depth_zero_max_levels() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, 5));
  ob.add(lim(2, Side::Sell, 110, 5));
  CHECK(ob.depth(Side::Buy, 0).empty());
  CHECK(ob.depth(Side::Sell, 0).empty());
  expect_book(ob, {lvl(100, 5)}, {lvl(110, 5)}, 2);
}

void test_depth_fewer_levels_than_max() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, 5));
  ob.add(lim(2, Side::Buy, 99, 3));
  ob.add(lim(3, Side::Sell, 105, 2));

  expect_levels(ob.depth(Side::Buy, 5), {lvl(100, 5), lvl(99, 3)});
  expect_levels(ob.depth(Side::Sell, 5), {lvl(105, 2)});
  // Exactly as many levels as requested.
  expect_levels(ob.depth(Side::Buy, 2), {lvl(100, 5), lvl(99, 3)});
  expect_levels(ob.depth(Side::Sell, 1), {lvl(105, 2)});
}

void test_depth_ordering_and_truncation() {
  OrderBook ob;
  // Bids: 101 (4), 100 (2 + 1 = 3), 99 (3), 98 (1).
  ob.add(lim(1, Side::Buy, 101, 4));
  ob.add(lim(2, Side::Buy, 100, 2));
  ob.add(lim(3, Side::Buy, 99, 3));
  ob.add(lim(4, Side::Buy, 98, 1));
  ob.add(lim(5, Side::Buy, 100, 1));
  // Asks: 103 (5), 104 (7), 105 (6), 106 (8). No crossing with the bids.
  ob.add(lim(6, Side::Sell, 105, 6));
  ob.add(lim(7, Side::Sell, 103, 5));
  ob.add(lim(8, Side::Sell, 104, 7));
  ob.add(lim(9, Side::Sell, 106, 8));

  // Bids descending, asks ascending.
  expect_levels(ob.depth(Side::Buy, 5),
                {lvl(101, 4), lvl(100, 3), lvl(99, 3), lvl(98, 1)});
  expect_levels(ob.depth(Side::Sell, 5),
                {lvl(103, 5), lvl(104, 7), lvl(105, 6), lvl(106, 8)});

  // Truncation keeps the best levels.
  expect_levels(ob.depth(Side::Buy, 2), {lvl(101, 4), lvl(100, 3)});
  expect_levels(ob.depth(Side::Sell, 3), {lvl(103, 5), lvl(104, 7), lvl(105, 6)});

  // depth volume agrees with volume_at.
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 3u);
  CHECK_EQ(ob.volume_at(Side::Sell, 104), 7u);
  CHECK_EQ(ob.order_count(), 9u);
}

void test_depth_after_partial_fill_and_cancel() {
  OrderBook ob;
  ob.add(lim(1, Side::Sell, 100, 10));
  ob.add(lim(2, Side::Sell, 101, 5));
  ob.add(lim(3, Side::Buy, 95, 4));

  // Market buy of 4 partially fills id1 (10 -> 6) at level 100.
  AddResult r = ob.add(mkt(4, Side::Buy, 4));
  expect_trades(r, {trd(4, 1, 100, 4)});
  expect_book(ob, {lvl(95, 4)}, {lvl(100, 6), lvl(101, 5)}, 3);

  // Cancel id2 removes the 101 level entirely.
  CHECK(ob.cancel(2));
  expect_book(ob, {lvl(95, 4)}, {lvl(100, 6)}, 2);

  // Cancel the only bid: the bid side is empty.
  CHECK(ob.cancel(3));
  expect_book(ob, {}, {lvl(100, 6)}, 1);
}

// ---- E1.3: amend ----

void test_amend_reduces_volume() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, 10));
  ob.add(lim(2, Side::Buy, 100, 5));
  ob.add(lim(3, Side::Buy, 99, 4));

  CHECK(ob.amend(2, 2));  // 5 -> 2 on a non-head order
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 12u);
  expect_book(ob, {lvl(100, 12), lvl(99, 4)}, {}, 3);

  CHECK(ob.amend(1, 1));  // head order 10 -> 1
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 3u);
  expect_book(ob, {lvl(100, 3), lvl(99, 4)}, {}, 3);
}

void test_amend_keeps_queue_position() {
  OrderBook ob;
  ob.add(lim(1, Side::Sell, 100, 5));
  ob.add(lim(2, Side::Sell, 100, 5));
  ob.add(lim(3, Side::Sell, 100, 5));

  // Head order id1 keeps the front of the queue after amend.
  CHECK(ob.amend(1, 2));
  expect_book(ob, {}, {lvl(100, 12)}, 3);
  AddResult r = ob.add(mkt(10, Side::Buy, 3));
  expect_trades(r, {trd(10, 1, 100, 2), trd(10, 2, 100, 1)});
  CHECK_EQ(r.remaining, 0u);
  expect_book(ob, {}, {lvl(100, 9)}, 2);

  // Non-head order id3 amended down also keeps its position (behind id2).
  CHECK(ob.amend(3, 1));
  r = ob.add(mkt(11, Side::Buy, 5));
  expect_trades(r, {trd(11, 2, 100, 4), trd(11, 3, 100, 1)});
  expect_book(ob, {}, {}, 0);
}

void test_amend_after_partial_fill() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, 10));
  ob.add(lim(2, Side::Sell, 100, 4));  // id1 now has 6 left
  CHECK(ob.amend(1, 3));
  expect_book(ob, {lvl(100, 3)}, {}, 1);

  AddResult r = ob.add(mkt(3, Side::Sell, 3));
  expect_trades(r, {trd(1, 3, 100, 3)});
  expect_book(ob, {}, {}, 0);
}

void test_amend_partial_fill_then_cancel() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, 10));
  ob.add(lim(2, Side::Sell, 100, 4));  // id1 remaining 6
  CHECK(ob.amend(1, 3));
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 3u);

  // Cancel removes the amended remaining qty (3), so the level empties.
  CHECK(ob.cancel(1));
  CHECK_EQ(ob.volume_at(Side::Buy, 100), 0u);
  expect_book(ob, {}, {}, 0);

  // The cancelled id can no longer be amended.
  CHECK(!ob.amend(1, 1));
  expect_book(ob, {}, {}, 0);
}

void test_depth_aggregates_orders_at_one_price() {
  OrderBook ob;
  ob.add(lim(1, Side::Sell, 100, 2));
  ob.add(lim(2, Side::Sell, 100, 3));
  ob.add(lim(3, Side::Sell, 100, 5));
  ob.add(lim(4, Side::Sell, 101, 1));
  expect_levels(ob.depth(Side::Sell, 5), {lvl(100, 10), lvl(101, 1)});

  CHECK(ob.amend(3, 4));  // level 100: 10 -> 6
  CHECK(ob.cancel(2));    // level 100: 6 -> 3
  expect_book(ob, {}, {lvl(100, 3), lvl(101, 1)}, 3);
  CHECK_EQ(ob.volume_at(Side::Sell, 100), 3u);
}

// ---- E1.3: amend failures change nothing ----

void test_amend_failures_fresh_order() {
  OrderBook ob;
  ob.add(lim(5, Side::Sell, 110, 8));
  CHECK(!ob.amend(5, 0));  // new_qty == 0
  CHECK(!ob.amend(5, 8));  // new_qty == current
  CHECK(!ob.amend(5, 9));  // new_qty > current
  CHECK(!ob.amend(5, kMaxQty));
  expect_book(ob, {}, {lvl(110, 8)}, 1);
}

void test_amend_failures_after_partial_fill() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, 10));
  ob.add(lim(2, Side::Sell, 100, 4));  // id1 remaining 6, id2 fully filled
  expect_book(ob, {lvl(100, 6)}, {}, 1);

  CHECK(!ob.amend(1, 0));
  CHECK(!ob.amend(1, 6));  // equal to remaining
  CHECK(!ob.amend(1, 7));  // above remaining, below original qty
  CHECK(!ob.amend(1, 10));  // the original qty is not a valid target
  CHECK(!ob.amend(1, kMaxU64));
  expect_book(ob, {lvl(100, 6)}, {}, 1);
}

void test_amend_failures_unknown_filled_cancelled() {
  OrderBook ob;
  CHECK(!ob.amend(999, 1));  // empty book, unknown id

  ob.add(lim(1, Side::Sell, 100, 5));
  ob.add(mkt(2, Side::Buy, 5));  // fully fills id1
  CHECK(!ob.amend(1, 3));        // filled id
  CHECK(!ob.amend(999, 1));      // unknown id, empty book again
  expect_book(ob, {}, {}, 0);

  ob.add(lim(3, Side::Buy, 90, 4));
  CHECK(ob.cancel(3));
  CHECK(!ob.amend(3, 2));  // cancelled id
  expect_book(ob, {}, {}, 0);
}

// ---- Extreme values ----

void test_extreme_prices() {
  OrderBook ob;
  CHECK(ob.add(lim(1, Side::Sell, kMaxPx, kMaxQty)).status == AddStatus::Accepted);
  CHECK(ob.best_ask() == kMaxPx);
  CHECK_EQ(ob.volume_at(Side::Sell, kMaxPx), kMaxQty);

  AddResult r = ob.add(lim(2, Side::Buy, kMaxPx, 1));
  expect_trades(r, {trd(2, 1, kMaxPx, 1)});
  CHECK_EQ(r.remaining, 0u);
  CHECK_EQ(ob.volume_at(Side::Sell, kMaxPx), kMaxQty - 1);

  // Smallest positive price is valid.
  CHECK(ob.add(lim(3, Side::Buy, 1, 1)).status == AddStatus::Accepted);
  CHECK(ob.best_bid() == 1);
}

void test_quantity_limit() {
  OrderBook ob;
  AddResult r = ob.add(lim(1, Side::Buy, 100, kMaxQty));
  CHECK(r.status == AddStatus::Accepted);  // exactly at the limit is allowed
  CHECK_EQ(r.remaining, kMaxQty);
  CHECK_EQ(ob.volume_at(Side::Buy, 100), kMaxQty);

  // C8 and C4: kMaxQty + 1 and UINT64_MAX are rejected, limit and market alike.
  Order over_lim = lim(2, Side::Buy, 100, kMaxQty + 1);
  expect_rejected(ob.add(over_lim), over_lim);
  Order max_lim = lim(3, Side::Buy, 100, kMaxU64);
  expect_rejected(ob.add(max_lim), max_lim);
  Order over_mkt = mkt(4, Side::Sell, kMaxQty + 1);
  expect_rejected(ob.add(over_mkt), over_mkt);
  Order max_mkt = mkt(5, Side::Sell, kMaxU64);
  expect_rejected(ob.add(max_mkt), max_mkt);

  // No state change from the rejects.
  CHECK_EQ(ob.order_count(), 1u);
  CHECK_EQ(ob.volume_at(Side::Buy, 100), kMaxQty);
  CHECK(!ob.best_ask());
}

void test_extreme_quantities() {
  OrderBook ob;
  ob.add(lim(1, Side::Buy, 100, kMaxQty));

  // Partial fill from a huge resting order.
  AddResult r = ob.add(mkt(2, Side::Sell, 1));
  expect_trades(r, {trd(1, 2, 100, 1)});
  CHECK_EQ(ob.volume_at(Side::Buy, 100), kMaxQty - 1);

  // Huge incoming limit: fills the 5 on the ask and rests the rest.
  ob.add(lim(3, Side::Sell, 200, 5));
  r = ob.add(lim(4, Side::Buy, 200, kMaxQty));
  expect_trades(r, {trd(4, 3, 200, 5)});
  CHECK_EQ(r.remaining, kMaxQty - 5);
  CHECK(ob.best_bid() == 200);
  CHECK_EQ(ob.volume_at(Side::Buy, 200), kMaxQty - 5);

  // Huge market order against an empty side is accepted and fully discarded.
  OrderBook empty;
  r = empty.add(mkt(10, Side::Buy, kMaxQty));
  CHECK(r.status == AddStatus::Accepted);
  CHECK(r.trades.empty());
  CHECK_EQ(r.remaining, kMaxQty);
  CHECK_EQ(empty.order_count(), 0u);
}

// ---- Randomized differential test against a brute-force reference ----

// Simple reference: a vector of resting orders in arrival order, scanned linearly.
struct RefBook {
  struct Resting {
    std::uint64_t id;
    Side side;
    std::int64_t price;
    std::uint64_t qty;
  };
  std::vector<Resting> orders;  // arrival order

  bool is_live(std::uint64_t id) const {
    for (const Resting& x : orders) {
      if (x.id == id) return true;
    }
    return false;
  }

  AddResult add(const Order& o) {
    AddResult r;
    r.status = AddStatus::Accepted;
    r.remaining = o.qty;
    if (o.qty == 0 || o.qty > kMaxQty || (o.type == OrderType::Limit && o.price <= 0) || is_live(o.id)) {
      r.status = AddStatus::Rejected;
      return r;
    }

    std::uint64_t rem = o.qty;
    while (rem > 0) {
      std::size_t best = orders.size();
      for (std::size_t i = 0; i < orders.size(); ++i) {
        const Resting& x = orders[i];
        if (x.side == o.side) continue;
        if (o.type == OrderType::Limit) {
          const bool crosses = (o.side == Side::Buy) ? (x.price <= o.price) : (x.price >= o.price);
          if (!crosses) continue;
        }
        if (best == orders.size()) {
          best = i;
          continue;
        }
        // Buy takes the lowest ask, Sell takes the highest bid. Ties keep the earlier arrival.
        const std::int64_t bp = orders[best].price;
        const bool better = (o.side == Side::Buy) ? (x.price < bp) : (x.price > bp);
        if (better) best = i;
      }
      if (best == orders.size()) break;

      Resting& x = orders[best];
      const std::uint64_t q = rem < x.qty ? rem : x.qty;
      Trade t;
      t.buy_id = (o.side == Side::Buy) ? o.id : x.id;
      t.sell_id = (o.side == Side::Buy) ? x.id : o.id;
      t.price = x.price;
      t.qty = q;
      r.trades.push_back(t);
      rem -= q;
      x.qty -= q;
      if (x.qty == 0) orders.erase(orders.begin() + static_cast<std::ptrdiff_t>(best));
    }

    r.remaining = rem;
    if (o.type == OrderType::Limit && rem > 0) orders.push_back({o.id, o.side, o.price, rem});
    return r;
  }

  bool cancel(std::uint64_t id) {
    for (std::size_t i = 0; i < orders.size(); ++i) {
      if (orders[i].id == id) {
        orders.erase(orders.begin() + static_cast<std::ptrdiff_t>(i));
        return true;
      }
    }
    return false;
  }

  std::optional<std::int64_t> best(Side s) const {
    std::optional<std::int64_t> b;
    for (const Resting& x : orders) {
      if (x.side != s) continue;
      if (!b || (s == Side::Buy ? x.price > *b : x.price < *b)) b = x.price;
    }
    return b;
  }

  std::uint64_t volume(Side s, std::int64_t price) const {
    std::uint64_t v = 0;
    for (const Resting& x : orders) {
      if (x.side == s && x.price == price) v += x.qty;
    }
    return v;
  }

  // E1.3: reduce-only, in place, so queue position is kept.
  bool amend(std::uint64_t id, std::uint64_t new_qty) {
    for (Resting& x : orders) {
      if (x.id != id) continue;
      if (new_qty == 0 || new_qty >= x.qty) return false;
      x.qty = new_qty;
      return true;
    }
    return false;
  }

  // E1.2: aggregate by price, best first, at most max_levels entries.
  std::vector<Level> depth(Side s, std::size_t max_levels) const {
    std::vector<Level> levels;
    for (const Resting& x : orders) {
      if (x.side != s) continue;
      bool found = false;
      for (Level& l : levels) {
        if (l.price == x.price) {
          l.volume += x.qty;
          found = true;
          break;
        }
      }
      if (!found) levels.push_back(lvl(x.price, x.qty));
    }
    // Stable insertion sort, best price first.
    for (std::size_t i = 1; i < levels.size(); ++i) {
      for (std::size_t j = i; j > 0; --j) {
        const bool better = (s == Side::Buy) ? (levels[j].price > levels[j - 1].price)
                                             : (levels[j].price < levels[j - 1].price);
        if (!better) break;
        std::swap(levels[j], levels[j - 1]);
      }
    }
    if (levels.size() > max_levels) levels.resize(max_levels);
    return levels;
  }
};

void compare_add(const AddResult& got, const AddResult& want) {
  CHECK(got.status == want.status);
  CHECK_EQ(got.remaining, want.remaining);
  expect_trades(got, want.trades);
}

void test_differential_random() {
  OrderBook ob;
  RefBook ref;
  std::mt19937_64 rng(20240601ULL);  // fixed seed: failures are reproducible
  auto pick = [&rng](std::uint64_t n) { return rng() % n; };

  const std::int64_t kPriceRange = 20;
  const std::size_t kOps = 5000;

  for (std::size_t op = 0; op < kOps; ++op) {
    const int failures_before = g_failures;

    const std::uint64_t kind = pick(10);
    if (kind < 6) {  // add
      Order o;
      o.id = 1 + pick(200);  // small id space forces duplicates and reuse
      o.side = pick(2) == 0 ? Side::Buy : Side::Sell;
      o.type = pick(100) < 15 ? OrderType::Market : OrderType::Limit;
      o.price = 1 + static_cast<std::int64_t>(pick(kPriceRange));
      if (pick(20) == 0) o.price = 0;  // invalid for Limit, ignored for Market
      const std::uint64_t roll = pick(100);
      if (roll < 3) {
        o.qty = 0;
      } else if (roll < 5) {
        o.qty = kMaxQty + 1;
      } else if (roll < 6) {
        o.qty = kMaxQty;
      } else {
        o.qty = 1 + pick(50);
      }

      const AddResult got = ob.add(o);
      const AddResult want = ref.add(o);
      compare_add(got, want);
    } else if (kind < 8) {  // amend: qty 0..51 covers reduce, equal, greater and zero
      const std::uint64_t id = 1 + pick(200);
      const std::uint64_t new_qty = pick(52);
      const bool got = ob.amend(id, new_qty);
      const bool want = ref.amend(id, new_qty);
      CHECK(got == want);
    } else {  // cancel
      const std::uint64_t id = 1 + pick(200);
      const bool got = ob.cancel(id);
      const bool want = ref.cancel(id);
      CHECK(got == want);
    }

    CHECK(ob.best_bid() == ref.best(Side::Buy));
    CHECK(ob.best_ask() == ref.best(Side::Sell));
    CHECK_EQ(ob.order_count(), ref.orders.size());
    for (std::int64_t p = 1; p <= kPriceRange; ++p) {
      CHECK_EQ(ob.volume_at(Side::Buy, p), ref.volume(Side::Buy, p));
      CHECK_EQ(ob.volume_at(Side::Sell, p), ref.volume(Side::Sell, p));
    }
    expect_levels(ob.depth(Side::Buy, 5), ref.depth(Side::Buy, 5));
    expect_levels(ob.depth(Side::Sell, 5), ref.depth(Side::Sell, 5));

    if (g_failures != failures_before) {
      std::fprintf(stderr, "differential test diverged at op %zu\n", op);
      return;
    }
  }
}

}  // namespace

int main() {
  test_fifo_within_level();
  test_cancel_from_middle_keeps_fifo();
  test_trade_price_is_resting_price();
  test_partial_fill_incoming_buy_remainder_rests();
  test_partial_fill_incoming_sell_remainder_rests();
  test_sweep_multiple_levels();
  test_market_remainder_discarded();
  test_market_price_is_ignored();
  test_reject_zero_qty();
  test_reject_non_positive_limit_price();
  test_reject_duplicate_live_id();
  test_reuse_id_after_fill();
  test_reuse_id_after_cancel();
  test_cancel_unknown_and_filled();
  test_cancel_twice_and_partially_filled();
  test_empty_levels_removed();
  test_volume_at_and_order_count();
  test_depth_empty_book();
  test_depth_zero_max_levels();
  test_depth_fewer_levels_than_max();
  test_depth_ordering_and_truncation();
  test_depth_after_partial_fill_and_cancel();
  test_depth_aggregates_orders_at_one_price();
  test_amend_reduces_volume();
  test_amend_keeps_queue_position();
  test_amend_after_partial_fill();
  test_amend_partial_fill_then_cancel();
  test_amend_failures_fresh_order();
  test_amend_failures_after_partial_fill();
  test_amend_failures_unknown_filled_cancelled();
  test_extreme_prices();
  test_quantity_limit();
  test_extreme_quantities();
  test_differential_random();

  std::printf("%d checks, %d failures\n", g_checks, g_failures);
  return g_failures == 0 ? 0 : 1;
}
