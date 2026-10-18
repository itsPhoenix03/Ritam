#pragma once

#include <cstdint>

namespace engine {

enum class Side { Buy, Sell };
enum class OrderType { Limit, Market };

inline constexpr std::uint64_t kMaxQty = 1'000'000'000'000ULL;

struct Order {
    std::uint64_t id;
    Side side;
    OrderType type;
    std::int64_t price;
    std::uint64_t qty;
};

struct Trade {
    std::uint64_t buy_id;
    std::uint64_t sell_id;
    std::int64_t price;
    std::uint64_t qty;
};

struct Level {
    std::int64_t price;
    std::uint64_t volume;
};

} // namespace engine
