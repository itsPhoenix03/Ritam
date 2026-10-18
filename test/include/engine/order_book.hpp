#pragma once

#include "engine/types.hpp"

#include <cstddef>
#include <list>
#include <map>
#include <optional>
#include <unordered_map>
#include <vector>

namespace engine {

enum class AddStatus { Accepted, Rejected };

struct AddResult {
    AddStatus status;
    std::vector<Trade> trades;
    std::uint64_t remaining;
};

class OrderBook {
public:
    OrderBook() = default;
    OrderBook(const OrderBook&) = delete;
    OrderBook& operator=(const OrderBook&) = delete;
    OrderBook(OrderBook&&) = delete;
    OrderBook& operator=(OrderBook&&) = delete;

    AddResult add(const Order& order);
    bool cancel(std::uint64_t id);
    std::optional<std::int64_t> best_bid() const;
    std::optional<std::int64_t> best_ask() const;
    std::uint64_t volume_at(Side side, std::int64_t price) const;
    std::size_t order_count() const;
    std::vector<Level> depth(Side side, std::size_t max_levels) const;
    bool amend(std::uint64_t id, std::uint64_t new_qty);

private:
    using Queue = std::list<Order>;
    struct PriceLevel {
        Queue orders;
        std::uint64_t volume = 0; // cached level total; assumed to fit in uint64_t (SPEC E1.6)
    };
    using Levels = std::map<std::int64_t, PriceLevel>;

    struct Locator {
        Side side;
        Levels::iterator level;
        Queue::iterator order;
    };

    Levels bids_;
    Levels asks_;
    std::unordered_map<std::uint64_t, Locator> live_;
};

} // namespace engine
