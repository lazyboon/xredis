-- rate_allow_multi_n.lua:
-- ARGV => [burst_1, rate_1, period_us_1, burst_2, rate_2, period_us_2, ..., cost]
-- KEYS => [rate_limit_key_1, rate_limit_key_2, ...]
--
-- Atomic multi-limit check for AllowN semantics:
-- 1) Check every limit first.
-- 2) If any limit rejects, do not update any key.
-- 3) If all pass, update all keys.
-- return => [allowed, remaining, retry_after, reset_after, limited_by_index]
-- limited_by_index is 1-based, -1 means no specific limiter.
-- remaining is the post-update remaining allowance after applying allowed.
-- retry_after is -1 when allowed > 0, otherwise it is the wait time for this requested cost.
-- reset_after is the duration until the limiting state is fully drained/reset.

local key_count = #KEYS
local cost = tonumber(ARGV[key_count * 3 + 1])

local now = redis.call("TIME")
local now_us = now[1] * 1000000 + now[2]

local new_tats = {}
local max_reset_after = 0
local min_remaining = nil
local deny_retry_after = 0
local deny_reset_after = 0
local deny_limited_index = -1
local denied = false

for i = 1, key_count do
  local arg_base = 1 + (i - 1) * 3
  local burst = tonumber(ARGV[arg_base])
  local rate = tonumber(ARGV[arg_base + 1])
  local period_us = tonumber(ARGV[arg_base + 2])

  local emission_interval = period_us / rate
  local increment = emission_interval * cost
  local burst_offset = emission_interval * burst

  local tat = redis.call("GET", KEYS[i])
  if not tat then
    tat = now_us
  else
    tat = tonumber(tat)
  end

  tat = math.max(tat, now_us)

  local new_tat = tat + increment
  local allow_at = new_tat - burst_offset
  local diff = now_us - allow_at
  local remaining = diff / emission_interval

  if diff < 0 then
    denied = true
    local reset_after = tat - now_us
    local retry_after = diff * -1
    if retry_after > deny_retry_after or (retry_after == deny_retry_after and reset_after > deny_reset_after) then
      deny_retry_after = retry_after
      deny_reset_after = reset_after
      deny_limited_index = i
    end
  else
    if min_remaining == nil or remaining < min_remaining then
      min_remaining = remaining
    end

    new_tats[i] = new_tat
    local reset_after = new_tat - now_us
    if reset_after > max_reset_after then
      max_reset_after = reset_after
    end
  end
end

if denied then
  return {0, 0, math.ceil(deny_retry_after), math.ceil(deny_reset_after), deny_limited_index}
end

for i = 1, key_count do
  local reset_after = new_tats[i] - now_us
  if reset_after > 0 then
    redis.call("SET", KEYS[i], new_tats[i], "PX", math.ceil(reset_after / 1000))
  end
end

return {cost, math.floor(min_remaining), -1, math.ceil(max_reset_after), -1}
