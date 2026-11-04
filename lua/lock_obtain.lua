-- ARGV: [stored value, ownership prefix length in bytes, TTL in milliseconds].
-- The ownership prefix is len(token) .. ":" .. token, preventing variable-length prefix collisions.
-- Acquire absent keys, or replace and renew them when every key has the same owner.

-- Apply one TTL to all keys within the atomic acquisition script.
local function pexpire(ttl)
  for _, key in ipairs(KEYS) do
    redis.call("pexpire", key, ttl)
  end
end

-- Compare the framed owner prefix only, allowing same-token retries to replace metadata.
local function canOverrideKeys()
  local offset = tonumber(ARGV[2])
  for _, key in ipairs(KEYS) do
    if redis.call("getrange", key, 0, offset - 1) ~= string.sub(ARGV[1], 1, offset) then
      return false
    end
  end
  return true
end

local setArgs = {}
for _, key in ipairs(KEYS) do
  table.insert(setArgs, key)
  table.insert(setArgs, ARGV[1])
end

-- MSETNX either creates every key or leaves all keys unchanged on contention.
if redis.call("msetnx", unpack(setArgs)) ~= 1 then
  if canOverrideKeys() == false then
    return false
  end
  redis.call("mset", unpack(setArgs))
end

pexpire(ARGV[3])
return redis.status_reply("OK")
