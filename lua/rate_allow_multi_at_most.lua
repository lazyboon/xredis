-- rate_allow_multi_at_most.lua:
-- ARGV => [burst_1, rate_1, period_us_1, burst_2, rate_2, period_us_2, ..., cost]
-- KEYS => [rate_limit_key_1, rate_limit_key_2, ...]
--
-- Atomic multi-limit check for AllowAtMost semantics:
-- 1) Compute max allowed cost for every limit.
-- 2) If any limit allows 0, reject without updating any key.
-- 3) Otherwise use the minimum allowed cost and update all keys.
-- return => [allowed, remaining, retry_after, reset_after, limited_by_index]
-- limited_by_index is 1-based, -1 means no specific limiter.
-- retry_after (when allowed=0) means time until at least 1 unit is allowed.
-- reset_after means time until the limiting state is fully drained/reset.
-- remaining is the post-update remaining allowance after applying allowed.

local key_count = #KEYS
local requested_cost = tonumber(ARGV[key_count * 3 + 1])

local now = redis.call("TIME")
local now_us = now[1] * 1000000 + now[2]

local tats = {}
local emission_intervals = {}
local burst_offsets = {}
local allowed_cost = requested_cost
local deny_retry_after = 0
local deny_reset_after = 0
local deny_limited_index = -1
local partial_limited_index = -1

for i = 1, key_count do
  local arg_base = 1 + (i - 1) * 3
  local burst = tonumber(ARGV[arg_base])
  local rate = tonumber(ARGV[arg_base + 1])
  local period_us = tonumber(ARGV[arg_base + 2])

  local emission_interval = period_us / rate
  local burst_offset = emission_interval * burst

  local tat = redis.call("GET", KEYS[i])
  if not tat then
    tat = now_us
  else
    tat = tonumber(tat)
  end

  tat = math.max(tat, now_us)

  local max_cost = math.floor((now_us - tat + burst_offset) / emission_interval)
  if max_cost < 0 then
    max_cost = 0
  elseif max_cost > requested_cost then
    max_cost = requested_cost
  end

  if max_cost <= 0 then
    local retry_after = tat + emission_interval - burst_offset - now_us
    if retry_after < 0 then
      retry_after = 0
    end
    local reset_after = tat - now_us
    if retry_after > deny_retry_after or (retry_after == deny_retry_after and reset_after > deny_reset_after) then
      deny_retry_after = retry_after
      deny_reset_after = reset_after
      deny_limited_index = i
    end
    allowed_cost = 0
  elseif max_cost < allowed_cost then
    allowed_cost = max_cost
    partial_limited_index = i
  end

  tats[i] = tat
  emission_intervals[i] = emission_interval
  burst_offsets[i] = burst_offset
end

if allowed_cost <= 0 then
  return {0, 0, math.ceil(deny_retry_after), math.ceil(deny_reset_after), deny_limited_index}
end

local min_remaining = nil
local max_reset_after = 0

for i = 1, key_count do
  local new_tat = tats[i] + emission_intervals[i] * allowed_cost
  local allow_at = new_tat - burst_offsets[i]
  local diff = now_us - allow_at
  local remaining = diff / emission_intervals[i]
  if min_remaining == nil or remaining < min_remaining then
    min_remaining = remaining
  end

  local reset_after = new_tat - now_us
  if reset_after > max_reset_after then
    max_reset_after = reset_after
  end
  if reset_after > 0 then
    redis.call("SET", KEYS[i], new_tat, "PX", math.ceil(reset_after / 1000))
  end
end

return {allowed_cost, math.floor(min_remaining), -1, math.ceil(max_reset_after), partial_limited_index}
