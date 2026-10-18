#include "engine/order_book.hpp"

#include <chrono>
#include <cstdint>
#include <exception>
#include <iomanip>
#include <iostream>
#include <random>

namespace {

void demo() {
    engine::OrderBook book;
    book.add({1, engine::Side::Sell, engine::OrderType::Limit, 100, 10});
    book.add({2, engine::Side::Sell, engine::OrderType::Limit, 101, 5});

    const auto result =
        book.add({3, engine::Side::Buy, engine::OrderType::Limit, 101, 12});

    std::cout << "Demo\n";
    for (const auto& trade : result.trades) {
        std::cout << "buy=" << trade.buy_id
                  << " sell=" << trade.sell_id
                  << " price=" << trade.price
                  << " qty=" << trade.qty << '\n';
    }

    std::cout << "remaining=" << result.remaining
              << " resting_orders=" << book.order_count() << '\n';

    if (const auto ask = book.best_ask()) {
        std::cout << "best_ask=" << *ask
                  << " volume=" << book.volume_at(engine::Side::Sell, *ask)
                  << '\n';
    }

    const auto print_depth = [&book](engine::Side side, const char* label) {
        std::cout << label << " depth (up to 5 levels):\n";
        const auto levels = book.depth(side, 5);
        if (levels.empty()) std::cout << "  empty\n";
        for (const auto& level : levels) {
            std::cout << "  price=" << level.price
                      << " volume=" << level.volume << '\n';
        }
    };
    print_depth(engine::Side::Sell, "Sell");
    print_depth(engine::Side::Buy, "Buy");

    // Order 2 has three units left after the incoming buy; reduce it to one.
    const auto before = book.volume_at(engine::Side::Sell, 101);
    const bool amended = book.amend(2, 1);
    std::cout << "amend id=2 new_qty=1 success=" << amended
              << " volume_at_101_before=" << before
              << " volume_at_101_after="
              << book.volume_at(engine::Side::Sell, 101) << '\n';
    print_depth(engine::Side::Sell, "Sell after amend");
}

void benchmark() {
    constexpr std::uint64_t order_total = 1'000'000;
    constexpr std::uint64_t seed = 0x5EED1234ULL;

    engine::OrderBook book;
    std::mt19937_64 random(seed);
    std::uint64_t trade_total = 0;
    std::uint64_t checksum = 0;
    std::uint64_t rejected_total = 0;
    std::uint64_t discarded_total = 0;

    const auto started = std::chrono::steady_clock::now();

    for (std::uint64_t id = 1; id <= order_total; ++id) {
        const auto side =
            (random() & 1ULL) != 0 ? engine::Side::Buy : engine::Side::Sell;
        const auto type =
            random() % 10 == 0 ? engine::OrderType::Market : engine::OrderType::Limit;
        const auto price = std::int64_t{9'900}
                         + static_cast<std::int64_t>(random() % 201);
        const auto quantity = std::uint64_t{1} + random() % 100;

        const auto result = book.add({id, side, type, price, quantity});
        trade_total += static_cast<std::uint64_t>(result.trades.size());
        for (const auto& trade : result.trades) {
            checksum += trade.qty * static_cast<std::uint64_t>(trade.price);
        }
        if (result.status == engine::AddStatus::Rejected) ++rejected_total;
        if (type == engine::OrderType::Market &&
            result.status == engine::AddStatus::Accepted) {
            discarded_total += result.remaining;
        }
    }

    const auto finished = std::chrono::steady_clock::now();
    const double elapsed =
        std::chrono::duration<double>(finished - started).count();

    std::cout << "\nBenchmark: " << order_total
              << " pseudo-random orders, seed=" << seed
              << "\nIncludes PRNG generation and add/result processing."
              << "\nElapsed seconds: " << std::fixed << std::setprecision(6)
              << elapsed
              << "\nOrders/sec: " << std::setprecision(0)
              << (elapsed > 0.0 ? static_cast<double>(order_total) / elapsed : 0.0)
              << "\nTrades: " << trade_total
              << "\nChecksum (sum of qty * price): " << checksum
              << "\nRejected orders: " << rejected_total
              << "\nDiscarded market quantity: " << discarded_total
              << "\nResting orders: " << book.order_count() << '\n';
}

} // namespace

int main() {
    try {
        demo();
        benchmark();
        return 0;
    } catch (const std::exception& error) {
        std::cerr << "Engine demo failed: " << error.what() << '\n';
        return 1;
    }
}
