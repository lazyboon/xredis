-- ARGV: [stored value].
-- Return the shortest positive TTL when all values match, or false on an ownership mismatch.
-- Return 0 if no matching key has a positive TTL.

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
