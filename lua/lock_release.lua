-- ARGV: [stored value].
-- Delete every key only if all stored values still match this lease.

-- Check ownership across the entire lease before deleting any key.
local values = redis.call("mget", unpack(KEYS))
for i, _ in ipairs(KEYS) do
  if values[i] ~= ARGV[1] then
    return false
  end
end

redis.call("del", unpack(KEYS))
return redis.status_reply("OK")
