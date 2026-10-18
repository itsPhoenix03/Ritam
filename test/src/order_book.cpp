#include "engine/order_book.hpp"
#include <algorithm>
#include <iterator>

namespace engine {

AddResult OrderBook::add(const Order& order) {
    if (order.qty == 0 || order.qty > kMaxQty ||
        (order.type == OrderType::Limit && order.price <= 0) ||
        live_.find(order.id) != live_.end()) {
        return {AddStatus::Rejected, {}, order.qty};
    }
    Order incoming = order;
    AddResult result{AddStatus::Accepted, {}, incoming.qty};
    auto& opposite = incoming.side == Side::Buy ? asks_ : bids_;
    while (incoming.qty != 0 && !opposite.empty()) {
        auto level = incoming.side == Side::Buy ? opposite.begin() : std::prev(opposite.end());
        if (incoming.type == OrderType::Limit &&
            (incoming.side == Side::Buy ? level->first > incoming.price : level->first < incoming.price)) {
            break;
        }
        auto& queue = level->second.orders;
        auto maker = queue.begin();
        const auto quantity = std::min(incoming.qty, maker->qty);
        result.trades.push_back(incoming.side == Side::Buy
            ? Trade{incoming.id, maker->id, level->first, quantity}
            : Trade{maker->id, incoming.id, level->first, quantity});
        incoming.qty -= quantity;
        maker->qty -= quantity;
        level->second.volume -= quantity;
        if (maker->qty == 0) {
            live_.erase(maker->id);
            queue.erase(maker);
            if (queue.empty()) opposite.erase(level);
        }
    }
    result.remaining = incoming.qty;
    if (incoming.type == OrderType::Limit && incoming.qty != 0) {
        auto& own = incoming.side == Side::Buy ? bids_ : asks_;
        auto [level, created] = own.try_emplace(incoming.price);
        (void)created;
        level->second.orders.push_back(incoming);
        level->second.volume += incoming.qty;
        live_.emplace(incoming.id, Locator{incoming.side, level, std::prev(level->second.orders.end())});
    }
    return result;
}

bool OrderBook::cancel(std::uint64_t id) {
    const auto found = live_.find(id);
    if (found == live_.end()) return false;

    const auto locator = found->second;
    auto& levels = locator.side == Side::Buy ? bids_ : asks_;
    auto& queue = locator.level->second.orders;
    locator.level->second.volume -= locator.order->qty;
    queue.erase(locator.order);
    live_.erase(found);
    if (queue.empty()) levels.erase(locator.level);
    return true;
}

std::optional<std::int64_t> OrderBook::best_bid() const {
    if (bids_.empty()) return std::nullopt;
    return bids_.rbegin()->first;
}

std::optional<std::int64_t> OrderBook::best_ask() const {
    if (asks_.empty()) return std::nullopt;
    return asks_.begin()->first;
}

std::uint64_t OrderBook::volume_at(Side side, std::int64_t price) const {
    const auto& levels = side == Side::Buy ? bids_ : asks_;
    const auto level = levels.find(price);
    if (level == levels.end()) return 0;

    return level->second.volume;
}

std::size_t OrderBook::order_count() const {
    return live_.size();
}

std::vector<Level> OrderBook::depth(Side side, std::size_t max_levels) const {
    std::vector<Level> result;
    if (side == Side::Buy) {
        for (auto it = bids_.rbegin(); it != bids_.rend() && result.size() < max_levels; ++it) {
            result.push_back(Level{it->first, it->second.volume});
        }
    } else {
        for (auto it = asks_.begin(); it != asks_.end() && result.size() < max_levels; ++it) {
            result.push_back(Level{it->first, it->second.volume});
        }
    }
    return result;
}

bool OrderBook::amend(std::uint64_t id, std::uint64_t new_qty) {
    const auto found = live_.find(id);
    if (found == live_.end()) return false;

    const auto& locator = found->second;
    const auto current = locator.order->qty;
    if (new_qty == 0 || new_qty >= current) return false;

    locator.level->second.volume -= current - new_qty;
    locator.order->qty = new_qty;
    return true;
}

} // namespace engine
