-- lock_pttl.lua: arguments => [value]
-- It returns the minimum positive TTL if all values match, otherwise false.

local values = redis.call("mget", unpack(KEYS))
for i, _ in ipairs(KEYS) do
  if values[i] ~= ARGV[1] then
    return false
  end
end

local minTTL = 0
for _, key in ipairs(KEYS) do
  local ttl = redis.call("pttl", key)
  if ttl > 0 and (minTTL == 0 or ttl < minTTL) then
    minTTL = ttl
  end
end

return minTTL
