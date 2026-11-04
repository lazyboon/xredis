-- ARGV: [burst, rate, period in microseconds for each rule, then request cost].
-- KEYS: one state key per rule, in the same order as ARGV.
--
-- AllowAtMost chooses the largest cost every rule permits, up to the requested cost.
-- A denial consumes no quota. Each key stores the theoretical arrival time (TAT) as Unix microseconds.
-- Result: [allowed cost, remaining quota, retry delay in microseconds,
--          recovery duration in microseconds, constraining rule index].
-- Rule indices are one-based; -1 means no rule constrained admission.
-- On denial, retry delay is the wait until at least one request can be admitted.

local key_count = #KEYS
local requested_cost = tonumber(ARGV[key_count * 3 + 1])

-- Use one server timestamp so every rule evaluates the same instant.
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

-- Determine the common admissible cost before writing any rule's state.
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

-- Apply the same admitted cost to every rule and retain state until its recovery time.
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
