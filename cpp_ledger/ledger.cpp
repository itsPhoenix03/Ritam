// ledger.cpp - multi-currency double-entry ledger engine.
//
// Amounts are stored as signed 64-bit integers in the currency's minor unit
// (cents for USD, yen for JPY, fils for BHD). Every journal entry is a set of
// postings that must net to zero per currency. Mutating operations take an
// idempotency key so that client retries do not double-post.
//
// Build: C++17 or later, e.g.  g++ -std=c++17 -O2 -pthread ledger.cpp -o ledger
//                              cl /std:c++17 /EHsc ledger.cpp

#include <algorithm>
#include <atomic>
#include <cctype>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <deque>
#include <initializer_list>
#include <iostream>
#include <limits>
#include <map>
#include <memory>
#include <mutex>
#include <shared_mutex>
#include <stdexcept>
#include <string>
#include <thread>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>

namespace ledger {

using Minor = std::int64_t;
using Clock = std::chrono::system_clock;        // wall time, journal timestamps only
using SteadyClock = std::chrono::steady_clock;  // intervals (idempotency TTL)

class LedgerError : public std::runtime_error {
public:
    using std::runtime_error::runtime_error;
};

struct Currency {
    std::string code;
    int exponent = 2;  // minor-unit digits: USD=2, JPY=0, BHD=3
};

// ---------------------------------------------------------------------------
// Arithmetic helpers
// ---------------------------------------------------------------------------

// Overflow-checked int64 arithmetic; throws instead of wrapping (UB).
inline Minor checked_add(Minor a, Minor b) {
    if ((b > 0 && a > std::numeric_limits<Minor>::max() - b) ||
        (b < 0 && a < std::numeric_limits<Minor>::min() - b)) {
        throw LedgerError("arithmetic overflow");
    }
    return a + b;
}

inline Minor checked_sub(Minor a, Minor b) {
    if ((b < 0 && a > std::numeric_limits<Minor>::max() + b) ||
        (b > 0 && a < std::numeric_limits<Minor>::min() + b)) {
        throw LedgerError("arithmetic overflow");
    }
    return a - b;
}

inline Minor checked_mul(Minor a, Minor b) {
    constexpr Minor kMax = std::numeric_limits<Minor>::max();
    constexpr Minor kMin = std::numeric_limits<Minor>::min();
    bool overflow;
    if (a > 0) {
        overflow = (b > 0) ? (a > kMax / b) : (b < kMin / a);
    } else {
        overflow = (b > 0) ? (a < kMin / b) : (a != 0 && b < kMax / a);
    }
    if (overflow) throw LedgerError("arithmetic overflow");
    return a * b;
}

// Divides numer by denom and rounds to the nearest integer, ties to even
// (banker's rounding), as required for regulatory FX reporting. Works for
// negative numerators: the result is symmetric, round(-x) == -round(x).
inline Minor round_half_even(Minor numer, Minor denom) {
    if (denom <= 0) throw LedgerError("round_half_even: denominator must be positive");
    Minor q = numer / denom;  // truncates toward zero
    Minor r = numer % denom;  // same sign as numer, |r| < denom
    if (r == 0) return q;
    Minor mag = r < 0 ? -r : r;
    Minor rest = denom - mag;  // no overflow: avoids computing mag * 2
    bool away = mag > rest || (mag == rest && (q % 2) != 0);
    if (away) q += (numer < 0) ? -1 : 1;
    return q;
}

inline Minor pow10(int n) {
    if (n < 0 || n > 18) throw LedgerError("pow10: exponent out of range");
    Minor v = 1;
    while (n-- > 0) v *= 10;
    return v;
}

// Converts `amount` (minor units of `from`) into minor units of `to`.
// rate_micros is the number of `to` major units per one `from` major unit,
// scaled by 1e6 (e.g. USD->JPY at 151.2345 is 151234500).
inline Minor convert(Minor amount, const Currency& from, const Currency& to, Minor rate_micros) {
    if (rate_micros <= 0) throw LedgerError("convert: rate must be positive");
    Minor scaled = checked_mul(amount, rate_micros);
    Minor denom = 1'000'000;
    int shift = to.exponent - from.exponent;
    if (shift >= 0) {
        scaled = checked_mul(scaled, pow10(shift));
    } else {
        denom *= pow10(-shift);
    }
    return round_half_even(scaled, denom);
}

// Account ids are case-insensitive and tolerate surrounding whitespace.
inline std::string normalize_id(std::string id) {
    id.erase(0, id.find_first_not_of(" \t"));
    id.erase(id.find_last_not_of(" \t") + 1);
    std::transform(id.begin(), id.end(), id.begin(),
                   [](unsigned char c) { return static_cast<char>(std::toupper(c)); });
    return id;
}

// ---------------------------------------------------------------------------
// Data model
// ---------------------------------------------------------------------------

struct Account {
    std::string id;
    std::string currency;
    Minor balance = 0;
    Minor overdraft_limit = 0;  // how far below zero the balance may go
    bool frozen = false;
    bool closed = false;  // set under `mu` by close_account; operations must reject closed accounts
    mutable std::mutex mu;
};

struct Posting {
    std::string account;  // normalized account id, or "EXTERNAL:<CCY>" / "FX:<CCY>"
    Minor amount = 0;     // positive = credit to account, negative = debit
    std::string currency;
};

struct JournalEntry {
    std::uint64_t seq = 0;
    std::uint64_t reverses = 0;  // seq of the entry this one compensates; 0 = not a reversal
    std::string idempotency_key;
    std::string memo;
    std::vector<Posting> postings;
    Clock::time_point at;
};

// One posting of an account's statement.
struct StatementLine {
    std::uint64_t seq = 0;
    Clock::time_point at;
    std::string memo;
    Minor amount = 0;           // signed, as posted
    Minor running_balance = 0;  // account balance after this posting
};

// ---------------------------------------------------------------------------
// Ledger
// ---------------------------------------------------------------------------

class Ledger {
public:
    explicit Ledger(std::chrono::seconds idempotency_ttl = std::chrono::hours(24))
        : idem_ttl_(idempotency_ttl) {}

    Ledger(const Ledger&) = delete;
    Ledger& operator=(const Ledger&) = delete;

    // ----- reference data --------------------------------------------------

    void register_currency(Currency c) {
        if (c.exponent < 0 || c.exponent > 4) throw LedgerError("unsupported currency exponent");
        if (c.code.empty()) throw LedgerError("currency code must not be empty");
        std::unique_lock lk(registry_mu_);
        auto it = currencies_.find(c.code);
        if (it != currencies_.end() && it->second.exponent != c.exponent) {
            // Existing balances and journal amounts are in the old minor unit.
            throw LedgerError("cannot change exponent of registered currency: " + c.code);
        }
        std::string code = c.code;
        currencies_[code] = std::move(c);
    }

    void set_rate(const std::string& from, const std::string& to, Minor rate_micros) {
        if (rate_micros <= 0) throw LedgerError("rate must be positive");
        std::unique_lock lk(registry_mu_);
        if (!currencies_.count(from)) throw LedgerError("unknown currency: " + from);
        if (!currencies_.count(to)) throw LedgerError("unknown currency: " + to);
        if (from == to) throw LedgerError("rate needs two different currencies");
        rates_[from + "/" + to] = rate_micros;
    }

    // ----- accounts --------------------------------------------------------

    void open_account(const std::string& raw_id, const std::string& currency,
                      Minor overdraft_limit = 0) {
        if (overdraft_limit < 0) throw LedgerError("overdraft limit must be non-negative");
        std::unique_lock lk(registry_mu_);
        if (!currencies_.count(currency)) throw LedgerError("unknown currency: " + currency);
        std::string id = normalize_id(raw_id);
        if (id.empty()) throw LedgerError("account id must not be empty");
        // ':' is reserved for the "EXTERNAL:<CCY>" / "FX:<CCY>" pseudo-accounts.
        if (id.find(':') != std::string::npos) throw LedgerError("account id must not contain ':'");
        if (accounts_.count(id)) throw LedgerError("account already exists: " + id);
        // Ids of closed accounts are retired for good: the journal records
        // postings by id, so reusing one would let the new account inherit the
        // old account's statement and reversals.
        if (retired_ids_.count(id)) throw LedgerError("account id was closed and cannot be reused: " + id);
        auto a = std::make_shared<Account>();
        a->id = id;
        a->currency = currency;
        a->overdraft_limit = overdraft_limit;
        accounts_.emplace(id, std::move(a));
    }

    // Never holds the registry lock and an account lock at the same time, so it
    // cannot deadlock against operations that hold account locks.
    void close_account(const std::string& raw_id) {
        std::shared_ptr<Account> a = find_account(raw_id);
        {
            std::lock_guard alk(a->mu);
            if (a->closed) throw LedgerError("unknown account: " + raw_id);
            if (a->balance != 0) {
                throw LedgerError("cannot close account with non-zero balance");
            }
            a->closed = true;  // in-flight operations holding `a` now fail
        }
        std::unique_lock lk(registry_mu_);
        auto it = accounts_.find(a->id);
        if (it != accounts_.end() && it->second == a) accounts_.erase(it);
        retired_ids_.insert(a->id);
    }

    void set_frozen(const std::string& raw_id, bool frozen) {
        std::shared_ptr<Account> a = find_account(raw_id);
        std::lock_guard lk(a->mu);
        if (a->closed) throw LedgerError("unknown account: " + raw_id);
        a->frozen = frozen;
    }

    Minor balance(const std::string& raw_id) {
        std::shared_ptr<Account> a = find_account(raw_id);
        std::lock_guard lk(a->mu);
        return a->balance;
    }

    // ----- money movement --------------------------------------------------

    // Credits an account from outside the ledger (card top-up, wire in, ...).
    std::uint64_t deposit(const std::string& raw_id, Minor amount, const std::string& idem_key) {
        if (amount <= 0) throw LedgerError("amount must be positive");
        return with_idempotency(idem_key, fingerprint("deposit", {normalize_id(raw_id)}, amount),
                                [&]() -> const JournalEntry& {
            std::shared_ptr<Account> ap = find_account(raw_id);
            Account& a = *ap;
            std::lock_guard lk(a.mu);
            if (a.closed) throw LedgerError("unknown account: " + raw_id);
            if (a.frozen) throw LedgerError("account is frozen: " + a.id);
            Minor new_balance = checked_add(a.balance, amount);
            // Append first: if it throws, the balance is untouched.
            const JournalEntry& e = append_entry(idem_key, "deposit",
                                {{a.id, amount, a.currency},
                                 {"EXTERNAL:" + a.currency, -amount, a.currency}});
            a.balance = new_balance;
            return e;
        });
    }

    // Debits an account to outside the ledger (payout, wire out, ...).
    std::uint64_t withdraw(const std::string& raw_id, Minor amount, const std::string& idem_key) {
        if (amount <= 0) throw LedgerError("amount must be positive");
        return with_idempotency(idem_key, fingerprint("withdraw", {normalize_id(raw_id)}, amount),
                                [&]() -> const JournalEntry& {
            std::shared_ptr<Account> ap = find_account(raw_id);
            Account& a = *ap;
            std::lock_guard lk(a.mu);
            if (a.closed) throw LedgerError("unknown account: " + raw_id);
            if (a.frozen) throw LedgerError("account is frozen: " + a.id);
            Minor new_balance = checked_sub(a.balance, amount);
            if (new_balance < -a.overdraft_limit) throw LedgerError("insufficient funds");
            const JournalEntry& e = append_entry(idem_key, "withdraw",
                                {{a.id, -amount, a.currency},
                                 {"EXTERNAL:" + a.currency, amount, a.currency}});
            a.balance = new_balance;
            return e;
        });
    }

    // Moves money between two accounts of the same currency.
    std::uint64_t transfer(const std::string& from_id, const std::string& to_id, Minor amount,
                           const std::string& idem_key) {
        if (amount <= 0) throw LedgerError("amount must be positive");
        // Ids are case/whitespace-insensitive: compare normalized forms, or "a1" vs
        // "A1" would lock the same mutex twice below.
        if (normalize_id(from_id) == normalize_id(to_id)) {
            throw LedgerError("cannot transfer to the same account");
        }
        return with_idempotency(idem_key,
                                fingerprint("transfer", {normalize_id(from_id), normalize_id(to_id)}, amount),
                                [&]() -> const JournalEntry& {
            std::shared_ptr<Account> fp = find_account(from_id);
            std::shared_ptr<Account> tp = find_account(to_id);
            Account& from = *fp;
            Account& to = *tp;
            std::scoped_lock lk(from.mu, to.mu);  // std::lock ordering, no deadlock
            if (from.closed) throw LedgerError("unknown account: " + from_id);
            if (to.closed) throw LedgerError("unknown account: " + to_id);
            if (from.frozen) throw LedgerError("account is frozen: " + from.id);
            if (to.frozen) throw LedgerError("account is frozen: " + to.id);
            if (from.currency != to.currency) {
                throw LedgerError("currency mismatch; use transfer_fx");
            }
            Minor from_new = checked_sub(from.balance, amount);
            if (from_new < -from.overdraft_limit) {
                throw LedgerError("insufficient funds");
            }
            Minor to_new = checked_add(to.balance, amount);
            const JournalEntry& e = append_entry(idem_key, "transfer",
                                {{from.id, -amount, from.currency},
                                 {to.id, amount, to.currency}});
            from.balance = from_new;
            to.balance = to_new;
            return e;
        });
    }

    // Moves `amount` (minor units of the source currency) to an account in a
    // different currency at the registered rate. FX:<CCY> clearing postings make
    // the entry net to zero in each currency.
    std::uint64_t transfer_fx(const std::string& from_id, const std::string& to_id,
                              Minor amount, const std::string& idem_key) {
        if (amount <= 0) throw LedgerError("transfer amount must be positive");
        if (normalize_id(from_id) == normalize_id(to_id))
            throw LedgerError("cannot transfer to the same account");

        return with_idempotency(idem_key,
                                fingerprint("transfer_fx", {normalize_id(from_id), normalize_id(to_id)}, amount),
                                [&]() -> const JournalEntry& {
            auto from_ptr = find_account(from_id);
            auto to_ptr = find_account(to_id);
            Account& from = *from_ptr;
            Account& to = *to_ptr;

            // Registry lookups must precede account locks.
            Currency from_currency = find_currency(from.currency);
            Currency to_currency = find_currency(to.currency);
            if (from.currency == to.currency)
                throw LedgerError("FX transfer requires different currencies");
            Minor rate = find_rate(from.currency, to.currency);
            Minor converted = convert(amount, from_currency, to_currency, rate);
            if (converted <= 0)
                throw LedgerError("FX amount rounds to zero");

            std::scoped_lock lk(from.mu, to.mu);
            if (from.closed || to.closed)
                throw LedgerError("unknown account");
            if (from.frozen || to.frozen)
                throw LedgerError("account is frozen");
            Minor next_from = checked_sub(from.balance, amount);
            if (next_from < checked_sub(0, from.overdraft_limit))
                throw LedgerError("overdraft limit exceeded");
            Minor next_to = checked_add(to.balance, converted);

            const JournalEntry& entry = append_entry(
                idem_key, "FX transfer", {
                    {from.id, checked_sub(0, amount), from.currency},
                    {"FX:" + from.currency, amount, from.currency},
                    {"FX:" + to.currency, checked_sub(0, converted), to.currency},
                    {to.id, converted, to.currency}
                });
            from.balance = next_from;
            to.balance = next_to;
            return entry;
        });
    }

    // Posts a compensating entry that undoes journal entry `seq` (FX entries
    // included) using the originally recorded amounts, never a fresh rate.
    // An entry can be reversed once; a reversal cannot be reversed. Fails if an
    // affected account is closed or frozen, or if undoing it would push an
    // account past its overdraft limit.
    std::uint64_t reverse(std::uint64_t seq, const std::string& idem_key) {
        return with_idempotency(idem_key, fingerprint("reverse", {}, seq),
                                [&]() -> const JournalEntry& {
            JournalEntry original = get_entry(seq);
            if (original.reverses != 0)
                throw LedgerError("cannot reverse a reversal");

            // Resolve accounts before taking any account lock. Account ids cannot
            // contain ':', so these prefixes unambiguously identify clearing legs.
            std::map<std::string, std::shared_ptr<Account>> accounts;
            for (const Posting& posting : original.postings) {
                if (posting.account.rfind("EXTERNAL:", 0) == 0 ||
                    posting.account.rfind("FX:", 0) == 0)
                    continue;
                auto account = find_account(posting.account);
                accounts.emplace(account->id, std::move(account));
            }

            // A fixed order also works alongside the two-account scoped locks.
            std::vector<std::unique_lock<std::mutex>> locks;
            locks.reserve(accounts.size());
            for (auto& pair : accounts)
                locks.emplace_back(pair.second->mu);

            std::map<std::string, Minor> next_balances;
            for (const auto& pair : accounts) {
                const Account& account = *pair.second;
                if (account.closed)
                    throw LedgerError("unknown account: " + account.id);
                if (account.frozen)
                    throw LedgerError("account is frozen: " + account.id);
                next_balances.emplace(pair.first, account.balance);
            }

            std::vector<Posting> postings;
            postings.reserve(original.postings.size());
            for (const Posting& posting : original.postings) {
                Minor opposite = checked_sub(0, posting.amount);
                postings.push_back({posting.account, opposite, posting.currency});
                auto account_it = accounts.find(posting.account);
                if (account_it == accounts.end()) continue;
                if (account_it->second->currency != posting.currency)
                    throw LedgerError("posting currency does not match account");
                Minor& balance = next_balances.at(posting.account);
                balance = checked_add(balance, opposite);
            }

            // A reversal is a new withdrawal where applicable; enforce overdraft.
            for (const auto& pair : accounts) {
                if (next_balances.at(pair.first) <
                    checked_sub(0, pair.second->overdraft_limit))
                    throw LedgerError("reversal exceeds overdraft limit");
            }

            const JournalEntry& entry = append_entry(
                idem_key, "reversal of " + std::to_string(seq),
                std::move(postings), seq);
            for (auto& pair : accounts)
                pair.second->balance = next_balances.at(pair.first);
            return entry;
        });
    }

    // The account's postings in journal order, each with the running balance
    // after that posting. Works from one journal snapshot, so it is internally
    // consistent; the last running_balance equals balance() as of that snapshot.
    std::vector<StatementLine> statement(const std::string& account_id) {
        std::string id = find_account(account_id)->id;  // throws if unknown
        std::vector<JournalEntry> snap = snapshot_journal();
        std::vector<StatementLine> lines;
        Minor running = 0;
        for (const JournalEntry& e : snap) {
            for (const Posting& p : e.postings) {
                if (p.account != id) continue;
                running = checked_add(running, p.amount);
                lines.push_back(StatementLine{e.seq, e.at, e.memo, p.amount, running});
            }
        }
        return lines;
    }

    // Audits the whole ledger and returns one message per problem found (empty
    // = consistent). Checks: journal sequence, per-currency zero-sum of every
    // entry, posting currency vs account currency, reversal links, and that each
    // account balance equals the sum of its postings. Briefly stops all
    // account mutation (every account lock is held, in id order) so balances
    // and the journal snapshot are mutually consistent.
    std::vector<std::string> verify() {
        // Registry (shared) is held for the whole audit, so no account can be
        // opened or closed between listing the accounts and taking the journal
        // snapshot. Order: registry -> account locks (id order) -> journal_mu_.
        // Nothing below calls find_*, so the shared lock is never re-acquired.
        std::shared_lock registry_lock(registry_mu_);
        std::vector<std::shared_ptr<Account>> accts;
        for (auto& kv : accounts_) accts.push_back(kv.second);
        std::sort(accts.begin(), accts.end(),
                  [](const std::shared_ptr<Account>& a, const std::shared_ptr<Account>& b) {
                      return a->id < b->id;
                  });
        std::vector<std::unique_lock<std::mutex>> locks;
        locks.reserve(accts.size());
        for (auto& a : accts) locks.emplace_back(a->mu);

        std::vector<JournalEntry> snap = snapshot_journal();
        std::vector<std::string> problems;
        std::unordered_map<std::string, Minor> posted;  // account id -> sum of postings
        std::unordered_map<std::uint64_t, std::uint64_t> reversal_of;  // target -> reversal seq

        for (std::size_t i = 0; i < snap.size(); ++i) {
            const JournalEntry& e = snap[i];
            const std::string tag = "entry " + std::to_string(e.seq) + ": ";
            if (e.seq != i + 1) problems.push_back(tag + "sequence gap or disorder");

            std::unordered_map<std::string, Minor> net;  // currency -> sum
            for (const Posting& p : e.postings) {
                try {
                    net[p.currency] = checked_add(net[p.currency], p.amount);
                    posted[p.account] = checked_add(posted[p.account], p.amount);
                } catch (const LedgerError&) {
                    problems.push_back(tag + "posting sum overflows");
                }
            }
            for (auto& kv : net) {
                if (kv.second != 0) {
                    problems.push_back(tag + "does not net to zero in " + kv.first + " (off by " +
                                       std::to_string(kv.second) + ")");
                }
            }
            if (e.postings.empty()) problems.push_back(tag + "has no postings");

            if (e.reverses != 0) {
                if (e.reverses >= e.seq || e.reverses > snap.size()) {
                    problems.push_back(tag + "reverses an invalid entry");
                } else {
                    const JournalEntry& orig = snap[e.reverses - 1];
                    if (orig.reverses != 0) problems.push_back(tag + "reverses a reversal");
                    if (!reversal_of.emplace(e.reverses, e.seq).second) {
                        problems.push_back(tag + "entry " + std::to_string(e.reverses) +
                                           " is reversed more than once");
                    }
                    // A reversal must exactly negate the original's postings.
                    bool mirror = orig.postings.size() == e.postings.size();
                    for (std::size_t k = 0; mirror && k < orig.postings.size(); ++k) {
                        const Posting& a = orig.postings[k];
                        const Posting& b = e.postings[k];
                        mirror = a.account == b.account && a.currency == b.currency &&
                                 a.amount != std::numeric_limits<Minor>::min() &&
                                 b.amount == -a.amount;
                    }
                    if (!mirror) problems.push_back(tag + "does not mirror entry " +
                                                    std::to_string(e.reverses));
                }
            }
        }

        std::unordered_set<std::string> live;
        for (auto& a : accts) {
            live.insert(a->id);
            if (a->closed) continue;
            Minor sum = 0;
            auto it = posted.find(a->id);
            if (it != posted.end()) sum = it->second;
            if (sum != a->balance) {
                problems.push_back("account " + a->id + ": balance " + std::to_string(a->balance) +
                                   " != sum of postings " + std::to_string(sum));
            }
        }
        std::unordered_map<std::string, std::string> ccy_of;  // live account id -> currency
        for (auto& a : accts) ccy_of[a->id] = a->currency;
        for (const JournalEntry& e : snap) {
            for (const Posting& p : e.postings) {
                auto it = ccy_of.find(p.account);
                if (it != ccy_of.end() && it->second != p.currency) {
                    problems.push_back("entry " + std::to_string(e.seq) + ": posting to " +
                                       p.account + " in " + p.currency + " but account is " +
                                       it->second);
                }
            }
        }
        // Postings to accounts that no longer exist (closed) must have netted to 0
        // (pseudo-accounts EXTERNAL:/FX: are allowed to hold any balance).
        for (auto& kv : posted) {
            const std::string& id = kv.first;
            if (live.count(id) || id.rfind("EXTERNAL:", 0) == 0 || id.rfind("FX:", 0) == 0) continue;
            if (kv.second != 0) {
                problems.push_back("closed/unknown account " + id + ": postings sum to " +
                                   std::to_string(kv.second));
            }
        }
        return problems;
    }

    std::size_t journal_size() const {
        std::lock_guard lk(journal_mu_);
        return journal_.size();
    }

private:
    struct IdemRecord {
        std::uint64_t seq = 0;
        bool in_flight = false;  // fn() for this key is still running
        SteadyClock::time_point at;
        std::string fingerprint;  // request that created the record
    };

    // Returns shared ownership so the account outlives a concurrent close_account.
    // Callers must check Account::closed under Account::mu before mutating.
    // Lock order: account mutexes (via scoped_lock) -> journal_mu_ -> idem_mu_;
    // never take registry_mu_ (find_*) while holding an account mutex.
    std::shared_ptr<Account> find_account(const std::string& raw_id) {
        std::shared_lock lk(registry_mu_);
        auto it = accounts_.find(normalize_id(raw_id));
        if (it == accounts_.end()) throw LedgerError("unknown account: " + raw_id);
        return it->second;
    }

    // Returned by value: a reference into the map would race with register_currency.
    Currency find_currency(const std::string& code) {
        std::shared_lock lk(registry_mu_);
        auto it = currencies_.find(code);
        if (it == currencies_.end()) throw LedgerError("unknown currency: " + code);
        return it->second;
    }

    Minor find_rate(const std::string& from, const std::string& to) {
        std::shared_lock lk(registry_mu_);
        auto it = rates_.find(from + "/" + to);
        if (it == rates_.end()) throw LedgerError("no rate for " + from + "/" + to);
        return it->second;
    }

    // Appends under journal_mu_ and returns a reference that stays valid
    // (journal_ is a deque; push_back never invalidates references).
    // Throws without appending if nothing can be appended. When `reverses` != 0
    // the check "target exists, is not itself a reversal, is not already
    // reversed" and the mark are atomic with the append, so an entry can be
    // reversed at most once even under concurrent reverse() calls.
    // Call it BEFORE mutating balances so a throw leaves state untouched.
    const JournalEntry& append_entry(const std::string& key, std::string memo,
                                     std::vector<Posting> postings,
                                     std::uint64_t reverses = 0) {
        std::lock_guard lk(journal_mu_);
        if (reverses != 0) {
            if (reverses >= next_seq_) throw LedgerError("unknown journal entry");
            if (journal_[reverses - 1].reverses != 0) {
                throw LedgerError("a reversal cannot be reversed");
            }
            if (reversed_.count(reverses)) throw LedgerError("entry already reversed");
        }
        JournalEntry e;
        e.seq = next_seq_;
        e.reverses = reverses;
        e.idempotency_key = key;
        e.memo = std::move(memo);
        e.postings = std::move(postings);
        e.at = Clock::now();
        journal_.push_back(std::move(e));
        ++next_seq_;
        if (reverses != 0) reversed_.insert(reverses);
        return journal_.back();
    }

    // Copy of entry `seq` (1-based); throws if it does not exist.
    JournalEntry get_entry(std::uint64_t seq) const {
        std::lock_guard lk(journal_mu_);
        if (seq == 0 || seq >= next_seq_) throw LedgerError("unknown journal entry");
        return journal_[seq - 1];
    }

    // Copy of the whole journal in seq order. Take it with no account lock held (journal_mu_ is ordered after them).
    std::vector<JournalEntry> snapshot_journal() const {
        std::lock_guard lk(journal_mu_);
        return std::vector<JournalEntry>(journal_.begin(), journal_.end());
    }

    // Canonical description of a request, compared when an idempotency key is
    // reused: operation, normalized account ids and amount (or target seq).
    // Ids are length-prefixed so that no id can imitate a separator.
    static std::string fingerprint(const char* op, std::initializer_list<std::string> ids,
                                   Minor amount) {
        std::string f = op;
        for (const std::string& id : ids) f += "|" + std::to_string(id.size()) + ":" + id;
        f += "|" + std::to_string(amount);
        return f;
    }

    // Same, for a journal sequence number (full unsigned range, no narrowing).
    static std::string fingerprint(const char* op, std::initializer_list<std::string> ids,
                                   std::uint64_t seq) {
        std::string f = op;
        for (const std::string& id : ids) f += "|" + std::to_string(id.size()) + ":" + id;
        f += "|" + std::to_string(seq);
        return f;
    }

    // Runs fn() at most once per idempotency key within the TTL window; a
    // retry with the same key AND the same request (`fp`, see fingerprint())
    // returns the sequence number of the original entry. A live key reused for
    // a different request throws LedgerError instead of replaying.
    // Concurrent calls with the same key wait for the first one to finish
    // rather than both executing. If fn() throws nothing is recorded and a
    // retry runs fn() again. Expired records are purged opportunistically.
    template <typename Fn>
    std::uint64_t with_idempotency(const std::string& key, const std::string& fp, Fn&& fn) {
        if (key.empty()) throw LedgerError("idempotency key is required");
        {
            std::unique_lock lk(idem_mu_);
            for (;;) {
                auto it = idem_.find(key);
                if (it == idem_.end()) break;
                bool live = it->second.in_flight ||
                            SteadyClock::now() - it->second.at < idem_ttl_;
                if (!live) break;  // expired: take over the key
                if (it->second.fingerprint != fp) {
                    throw LedgerError("idempotency key reused with different request");
                }
                if (!it->second.in_flight) return it->second.seq;
                idem_cv_.wait(lk);  // re-check: the owner may have failed
            }
            if (idem_.size() >= idem_purge_at_) {
                SteadyClock::time_point now = SteadyClock::now();
                for (auto it = idem_.begin(); it != idem_.end();) {
                    if (!it->second.in_flight && now - it->second.at >= idem_ttl_) {
                        it = idem_.erase(it);
                    } else {
                        ++it;
                    }
                }
                idem_purge_at_ = std::max<std::size_t>(1024, idem_.size() * 2);
            }
            idem_[key] = IdemRecord{0, true, SteadyClock::now(), fp};
        }
        std::uint64_t seq;
        try {
            seq = fn().seq;
        } catch (...) {
            {
                std::lock_guard lk(idem_mu_);
                idem_.erase(key);
            }
            idem_cv_.notify_all();
            throw;
        }
        {
            std::lock_guard lk(idem_mu_);
            idem_[key] = IdemRecord{seq, false, SteadyClock::now(), fp};
        }
        idem_cv_.notify_all();
        return seq;
    }

    std::chrono::seconds idem_ttl_;

    mutable std::shared_mutex registry_mu_;
    std::unordered_map<std::string, Currency> currencies_;
    std::unordered_map<std::string, Minor> rates_;
    std::unordered_map<std::string, std::shared_ptr<Account>> accounts_;
    std::unordered_set<std::string> retired_ids_;  // closed account ids, never reusable

    mutable std::mutex journal_mu_;
    std::deque<JournalEntry> journal_;           // journal_[seq - 1]
    std::unordered_set<std::uint64_t> reversed_;  // seqs that already have a reversal
    std::uint64_t next_seq_ = 1;

    std::mutex idem_mu_;
    std::condition_variable idem_cv_;
    std::unordered_map<std::string, IdemRecord> idem_;
    std::size_t idem_purge_at_ = 1024;  // purge expired records once idem_ reaches this size
};

}  // namespace ledger

// ---------------------------------------------------------------------------
// Demo and self-checking tests. Exits non-zero if any check fails.
// ---------------------------------------------------------------------------

namespace {

using namespace ledger;

int g_failures = 0;

#define CHECK(cond)                                                              \
    do {                                                                         \
        if (!(cond)) {                                                           \
            ++g_failures;                                                        \
            std::cerr << "FAIL " << __FILE__ << ":" << __LINE__ << ": " #cond << "\n"; \
        }                                                                        \
    } while (0)

// True if f() throws LedgerError; any other exception propagates (and fails the run).
template <typename F>
bool throws(F&& f) {
    try {
        f();
    } catch (const LedgerError&) {
        return true;
    }
    return false;
}

void check_verify(Ledger& l, const char* where) {
    std::vector<std::string> problems = l.verify();
    for (const std::string& p : problems) std::cerr << "verify(" << where << "): " << p << "\n";
    CHECK(problems.empty());
}

std::unique_ptr<Ledger> make_ledger(std::chrono::seconds ttl = std::chrono::hours(24)) {
    auto l = std::make_unique<Ledger>(ttl);
    l->register_currency({"USD", 2});
    l->register_currency({"JPY", 0});
    l->register_currency({"BHD", 3});
    l->set_rate("USD", "JPY", 151'234'500);  // 151.2345
    l->set_rate("JPY", "USD", 6'612);        // 0.006612
    l->set_rate("USD", "BHD", 377'000);      // 0.377
    l->set_rate("BHD", "USD", 2'652'520);    // 2.65252
    l->open_account("alice", "USD");
    l->open_account("bob", "USD");
    l->open_account("kenji", "JPY");
    l->open_account("hamad", "BHD");
    l->deposit("alice", 100'000, "seed-alice");  // 1000.00 USD
    return l;
}

void test_rounding() {
    CHECK(round_half_even(5, 2) == 2);    // 2.5 -> 2
    CHECK(round_half_even(7, 2) == 4);    // 3.5 -> 4
    CHECK(round_half_even(-5, 2) == -2);  // -2.5 -> -2
    CHECK(round_half_even(-7, 2) == -4);  // -3.5 -> -4
    CHECK(round_half_even(-3, 2) == -2);  // -1.5 -> -2
    CHECK(round_half_even(1, 3) == 0);
    CHECK(round_half_even(2, 3) == 1);
    CHECK(round_half_even(-1, 3) == 0);
    CHECK(round_half_even(-2, 3) == -1);
    CHECK(round_half_even(0, 7) == 0);
    CHECK(throws([] { round_half_even(1, 0); }));

    Currency usd{"USD", 2}, jpy{"JPY", 0}, bhd{"BHD", 3};
    CHECK(convert(10'000, usd, jpy, 151'234'500) == 15'123);  // 100.00 USD -> 15123.45 JPY
    CHECK(convert(10'000, usd, bhd, 377'000) == 37'700);      // 100.00 USD -> 37.700 BHD
    CHECK(convert(-10'000, usd, jpy, 151'234'500) == -15'123);  // symmetric for negatives
    CHECK(convert(10'000, usd, jpy, 150'000'000) == 15'000);
    CHECK(throws([&] { convert(1, usd, jpy, 0); }));
    CHECK(throws([&] { convert(std::numeric_limits<Minor>::max(), usd, jpy, 151'234'500); }));
}

void test_idempotency() {
    auto l = make_ledger();
    std::uint64_t a = l->deposit("bob", 1'000, "k1");
    std::uint64_t b = l->deposit("bob", 1'000, "k1");
    CHECK(a == b);
    CHECK(l->balance("bob") == 1'000);
    CHECK(throws([&] { l->deposit("bob", 1, ""); }));
    // A failed call is not remembered: retrying with the same key runs again.
    CHECK(throws([&] { l->withdraw("bob", 5'000, "k2"); }));
    l->deposit("bob", 10'000, "k3");
    l->withdraw("bob", 5'000, "k2");
    CHECK(l->balance("bob") == 6'000);

    // Expired keys are re-executed (TTL 0 => every call is a new posting).
    auto z = make_ledger(std::chrono::seconds(0));
    z->deposit("bob", 100, "same");
    z->deposit("bob", 100, "same");
    CHECK(z->balance("bob") == 200);
    check_verify(*l, "idempotency");
    check_verify(*z, "idempotency-ttl0");
}

void test_validation() {
    auto l = make_ledger();
    CHECK(throws([&] { l->transfer("alice", "ALICE", 1, "v1"); }));
    CHECK(throws([&] { l->transfer("alice", "  Alice ", 1, "v2"); }));
    CHECK(throws([&] { l->transfer_fx("alice", " aLiCe", 1, "v3"); }));
    CHECK(throws([&] { l->transfer("alice", "bob", 0, "v4"); }));
    CHECK(throws([&] { l->transfer("alice", "bob", -5, "v5"); }));
    CHECK(throws([&] { l->transfer("alice", "bob", 100'001, "v6"); }));  // insufficient funds
    CHECK(throws([&] { l->transfer("alice", "nobody", 1, "v7"); }));
    CHECK(throws([&] { l->transfer("alice", "kenji", 1, "v8"); }));      // needs transfer_fx
    CHECK(throws([&] { l->transfer_fx("alice", "bob", 1, "v9"); }));     // same currency
    CHECK(throws([&] { l->transfer_fx("kenji", "hamad", 1, "v10"); }));  // no JPY/BHD rate
    CHECK(throws([&] { l->open_account("fx:usd", "USD"); }));            // reserved ':'
    CHECK(throws([&] { l->open_account("Alice ", "USD"); }));            // duplicate, normalized
    CHECK(throws([&] { l->open_account("zed", "EUR"); }));               // unknown currency
    CHECK(throws([&] { l->register_currency({"USD", 3}); }));            // exponent change
    CHECK(l->journal_size() == 1);  // none of the above posted anything
    CHECK(l->balance("alice") == 100'000);

    l->set_frozen("bob", true);
    CHECK(throws([&] { l->transfer("alice", "bob", 1, "f1"); }));
    CHECK(throws([&] { l->deposit("bob", 1, "f2"); }));
    l->set_frozen("bob", false);
    l->transfer("alice", "bob", 1, "f3");
    CHECK(l->balance("bob") == 1);

    // Overdraft: carol may go to -500 and not beyond.
    l->open_account("carol", "USD", 500);
    l->deposit("carol", 100, "c1");
    l->withdraw("carol", 600, "c2");
    CHECK(l->balance("carol") == -500);
    CHECK(throws([&] { l->withdraw("carol", 1, "c3"); }));

    // Overflow must throw, not wrap, and must leave no trace.
    std::size_t before = l->journal_size();
    CHECK(throws([&] { l->deposit("alice", std::numeric_limits<Minor>::max(), "o1"); }));
    CHECK(l->journal_size() == before);
    CHECK(l->balance("alice") == 99'999);
    check_verify(*l, "validation");
}

void test_fx_and_statement() {
    auto l = make_ledger();
    std::uint64_t s1 = l->transfer_fx("alice", "kenji", 10'000, "fx1");
    std::uint64_t s2 = l->transfer_fx("alice", "hamad", 10'000, "fx2");
    CHECK(s1 != s2);
    CHECK(l->balance("alice") == 80'000);
    CHECK(l->balance("kenji") == 15'123);
    CHECK(l->balance("hamad") == 37'700);
    CHECK(l->transfer_fx("alice", "kenji", 10'000, "fx1") == s1);  // retry: no double post
    CHECK(l->balance("kenji") == 15'123);
    CHECK(throws([&] { l->transfer_fx("hamad", "alice", 1, "fx-tiny"); }));  // 0.001 BHD -> 0 cents
    CHECK(l->balance("hamad") == 37'700);

    std::vector<StatementLine> st = l->statement("KENJI");
    CHECK(st.size() == 1);
    if (st.size() == 1) {
        CHECK(st[0].amount == 15'123 && st[0].running_balance == 15'123 && st[0].seq == s1);
    }
    std::vector<StatementLine> sa = l->statement("alice");
    CHECK(sa.size() == 3);
    if (sa.size() == 3) {
        CHECK(sa[0].running_balance == 100'000);
        CHECK(sa[1].running_balance == 90'000);
        CHECK(sa[2].running_balance == 80'000);
        CHECK(sa.back().running_balance == l->balance("alice"));
    }
    CHECK(throws([&] { l->statement("nobody"); }));
    check_verify(*l, "fx");
}

void test_reverse() {
    auto l = make_ledger();
    std::uint64_t fx = l->transfer_fx("alice", "kenji", 10'000, "r-fx");
    std::uint64_t tr = l->transfer("alice", "bob", 25'000, "r-tr");
    std::uint64_t dp = l->deposit("bob", 700, "r-dp");

    std::uint64_t r1 = l->reverse(fx, "rev-fx");
    CHECK(r1 > tr);
    CHECK(l->reverse(fx, "rev-fx") == r1);          // idempotent retry
    CHECK(l->balance("kenji") == 0);                // restored from recorded amounts
    CHECK(l->balance("alice") == 75'000);
    CHECK(throws([&] { l->reverse(fx, "rev-fx-again"); }));  // only once
    CHECK(throws([&] { l->reverse(r1, "rev-of-rev"); }));    // reversals are final
    CHECK(throws([&] { l->reverse(9'999, "rev-unknown"); }));
    CHECK(throws([&] { l->reverse(0, "rev-zero"); }));

    l->reverse(tr, "rev-tr");
    CHECK(l->balance("alice") == 100'000);
    CHECK(l->balance("bob") == 700);
    l->reverse(dp, "rev-dp");
    CHECK(l->balance("bob") == 0);
    std::vector<StatementLine> sk = l->statement("kenji");
    CHECK(sk.size() == 2);
    if (sk.size() == 2) CHECK(sk[0].running_balance == 15'123 && sk[1].running_balance == 0);
    check_verify(*l, "reverse");

    // Reversal that would breach the overdraft limit is refused and changes nothing.
    std::uint64_t fx2 = l->transfer_fx("alice", "kenji", 10'000, "r-fx2");
    l->withdraw("kenji", 15'000, "r-w");
    std::size_t before = l->journal_size();
    CHECK(throws([&] { l->reverse(fx2, "rev-fx2"); }));
    CHECK(l->journal_size() == before);
    CHECK(l->balance("kenji") == 123);
    CHECK(l->balance("alice") == 90'000);
    // ...and a refused reversal does not consume the "once" allowance or the key.
    l->deposit("kenji", 15'000, "r-w-back");
    l->reverse(fx2, "rev-fx2");
    CHECK(l->balance("kenji") == 0);
    CHECK(l->balance("alice") == 100'000);
    check_verify(*l, "reverse-overdraft");

    // Frozen / closed accounts block a reversal.
    auto m = make_ledger();
    std::uint64_t t = m->transfer("alice", "bob", 100, "m-t");
    m->set_frozen("bob", true);
    CHECK(throws([&] { m->reverse(t, "m-rev") ; }));
    m->set_frozen("bob", false);
    m->reverse(t, "m-rev2");
    CHECK(m->balance("bob") == 0);
    check_verify(*m, "reverse-frozen");
}

void test_close() {
    auto l = make_ledger();
    CHECK(throws([&] { l->close_account("alice"); }));  // non-zero balance
    l->close_account("bob");
    CHECK(throws([&] { l->deposit("bob", 1, "cl1"); }));
    CHECK(throws([&] { l->balance("bob"); }));
    CHECK(throws([&] { l->close_account("bob"); }));
    // A closed id is retired: reopening would let a new account inherit the old
    // one's postings (statements, reversals of its entries).
    CHECK(throws([&] { l->open_account("bob", "USD"); }));
    CHECK(throws([&] { l->open_account(" BOB", "USD"); }));
    CHECK(throws([&] { l->statement("bob"); }));
    check_verify(*l, "close");

    // Entries made against a closed account can no longer be reversed.
    auto m = make_ledger();
    std::uint64_t t = m->transfer("alice", "bob", 300, "cl-t");
    m->transfer("bob", "alice", 300, "cl-back");
    m->close_account("bob");
    CHECK(throws([&] { m->reverse(t, "cl-rev") ; }));
    CHECK(m->balance("alice") == 100'000);
    check_verify(*m, "close-reverse");
}

void test_idempotency_fingerprint() {
    auto l = make_ledger();
    std::uint64_t s = l->deposit("bob", 100, "fp");
    CHECK(l->deposit(" BOB", 100, "fp") == s);                      // identical request (normalized id)
    CHECK(throws([&] { l->deposit("bob", 101, "fp"); }));           // different amount
    CHECK(throws([&] { l->deposit("alice", 100, "fp"); }));         // different account
    CHECK(throws([&] { l->withdraw("bob", 100, "fp"); }));          // different operation
    CHECK(throws([&] { l->transfer("alice", "bob", 100, "fp"); }));
    CHECK(throws([&] { l->transfer_fx("alice", "kenji", 100, "fp"); }));
    CHECK(throws([&] { l->reverse(s, "fp"); }));
    CHECK(l->balance("bob") == 100);
    CHECK(l->journal_size() == 2);

    std::uint64_t t = l->transfer("alice", "bob", 50, "fp-t");
    CHECK(throws([&] { l->transfer("bob", "alice", 50, "fp-t"); }));  // direction matters
    CHECK(l->transfer("alice", "bob", 50, "fp-t") == t);
    std::uint64_t r = l->reverse(t, "fp-r");
    CHECK(l->reverse(t, "fp-r") == r);
    CHECK(throws([&] { l->reverse(s, "fp-r"); }));                    // different target
    CHECK(l->balance("bob") == 100);
    check_verify(*l, "fingerprint");

    // An expired key may be reused for a different request.
    auto z = make_ledger(std::chrono::seconds(0));
    z->deposit("bob", 100, "reuse");
    z->deposit("bob", 7, "reuse");
    CHECK(z->balance("bob") == 107);

    // Many expired keys: exercises the opportunistic purge path.
    for (int i = 0; i < 3'000; ++i) z->deposit("bob", 1, "bulk-" + std::to_string(i));
    CHECK(z->balance("bob") == 3'107);
    check_verify(*z, "purge");
}

void test_set_rate() {
    auto l = make_ledger();
    CHECK(throws([&] { l->set_rate("USD", "EUR", 1'000'000); }));
    CHECK(throws([&] { l->set_rate("EUR", "USD", 1'000'000); }));
    CHECK(throws([&] { l->set_rate("USD", "USD", 1'000'000); }));
    CHECK(throws([&] { l->set_rate("USD", "JPY", 0); }));
    l->register_currency({"EUR", 2});
    l->set_rate("USD", "EUR", 920'000);
    l->open_account("eve", "EUR");
    l->transfer_fx("alice", "eve", 10'000, "eur1");
    CHECK(l->balance("eve") == 9'200);
}

// verify() while accounts are being opened and funded must never report a
// problem: the account set and the journal snapshot have to agree.
void test_verify_under_concurrency() {
    auto l = make_ledger();
    std::atomic<bool> done{false};
    std::thread writer([&] {
        for (int i = 0; i < 300; ++i) {
            std::string id = "acct" + std::to_string(i);
            l->open_account(id, "USD");
            l->deposit(id, 10, "dep-" + id);
            if (i % 3 == 0) l->transfer(id, "bob", 4, "tr-" + id);
        }
        done = true;
    });
    int audits = 0;
    while (!done.load() || audits < 5) {
        check_verify(*l, "concurrent-open");
        ++audits;
    }
    writer.join();
    check_verify(*l, "concurrent-open-final");
}

void test_concurrency() {
    // Same key from many threads: exactly one posting, all callers see one seq.
    {
        auto l = make_ledger();
        constexpr int kThreads = 16;
        std::vector<std::thread> ts;
        std::vector<std::uint64_t> seqs(kThreads, 0);
        for (int i = 0; i < kThreads; ++i) {
            ts.emplace_back([&, i] { seqs[i] = l->deposit("bob", 100, "shared-key"); });
        }
        for (auto& t : ts) t.join();
        for (int i = 1; i < kThreads; ++i) CHECK(seqs[i] == seqs[0]);
        CHECK(l->balance("bob") == 100);
        CHECK(l->journal_size() == 2);  // seed + one deposit
    }
    // Opposing transfers with differently-cased ids: no deadlock, money conserved.
    {
        auto l = make_ledger();
        l->deposit("bob", 100'000, "seed-bob");
        constexpr int kThreads = 8, kOps = 200;
        std::atomic<int> ok{0};
        std::vector<std::thread> ts;
        for (int i = 0; i < kThreads; ++i) {
            ts.emplace_back([&, i] {
                for (int j = 0; j < kOps; ++j) {
                    std::string key = "t" + std::to_string(i) + "-" + std::to_string(j);
                    try {
                        if (i % 2 == 0) l->transfer("alice", "BOB", 3, key);
                        else if (j % 4 == 0) l->transfer_fx("bob", "kenji", 100, key);
                        else l->transfer(" bob", "Alice", 2, key);
                        ++ok;
                    } catch (const LedgerError&) {
                    }
                }
            });
        }
        for (auto& t : ts) t.join();
        CHECK(ok.load() > 0);
        // FX legs convert some USD to JPY, so USD can only shrink.
        CHECK(l->balance("alice") + l->balance("bob") <= 200'000);
        check_verify(*l, "concurrent-transfers");
    }
    // Racing reversals of one entry: exactly one wins.
    {
        auto l = make_ledger();
        std::uint64_t t = l->transfer("alice", "bob", 1'000, "race-t");
        std::atomic<int> wins{0};
        std::vector<std::thread> ts;
        for (int i = 0; i < 8; ++i) {
            ts.emplace_back([&, i] {
                try {
                    l->reverse(t, "race-rev-" + std::to_string(i));
                    ++wins;
                } catch (const LedgerError&) {
                }
            });
        }
        for (auto& th : ts) th.join();
        CHECK(wins.load() == 1);
        CHECK(l->balance("alice") == 100'000);
        CHECK(l->balance("bob") == 0);
        check_verify(*l, "racing-reversals");
    }
}

void demo() {
    auto l = make_ledger();
    l->transfer("alice", "bob", 25'000, "demo-t1");
    std::uint64_t fx = l->transfer_fx("alice", "kenji", 10'000, "demo-fx1");
    l->transfer_fx("alice", "hamad", 10'000, "demo-fx2");
    l->reverse(fx, "demo-rev1");

    std::cout << "balances (minor units): alice=" << l->balance("alice")
              << " bob=" << l->balance("bob") << " kenji=" << l->balance("kenji")
              << " hamad=" << l->balance("hamad") << "\n";
    for (const char* who : {"alice", "kenji", "hamad"}) {
        std::cout << "statement " << who << ":\n";
        for (const StatementLine& s : l->statement(who)) {
            std::cout << "  #" << s.seq << " " << s.memo << " " << s.amount
                      << " -> " << s.running_balance << "\n";
        }
    }
    std::vector<std::string> problems = l->verify();
    std::cout << "verify: " << (problems.empty() ? "ok" : "PROBLEMS") << "\n";
    CHECK(problems.empty());
}

}  // namespace

int main() {
    try {
        demo();
        test_rounding();
        test_idempotency();
        test_validation();
        test_fx_and_statement();
        test_reverse();
        test_close();
        test_idempotency_fingerprint();
        test_set_rate();
        test_verify_under_concurrency();
        test_concurrency();
    } catch (const std::exception& e) {
        std::cerr << "unexpected exception: " << e.what() << "\n";
        return 2;
    }
    if (g_failures != 0) {
        std::cerr << g_failures << " check(s) failed\n";
        return 1;
    }
    std::cout << "all checks passed\n";
    return 0;
}
