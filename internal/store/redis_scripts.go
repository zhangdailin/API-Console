package store

import "github.com/redis/go-redis/v9"

// Every multi-step account, model and API-key mutation below runs as one Lua
// script so the read-modify-write it performs is atomic on the Redis side. The
// store keeps no lock of its own for these: two writers are serialized by Redis
// executing the script as a unit, which is what makes concurrent quota,
// billing and stat updates safe without a distributed lock.

var (
	consumeApiKeyRPMScript = redis.NewScript(`
		local value = redis.call("GET", KEYS[2])
		if not value then return redis.error_reply("api key not found") end
		local api_key = cjson.decode(value)
		api_key.last_used_at = ARGV[2]
		redis.call("SET", KEYS[2], cjson.encode(api_key))
		local limit = tonumber(ARGV[3])
		if limit <= 0 then return 0 end
		local count = redis.call("INCR", KEYS[1])
		if count == 1 then
			redis.call("EXPIRE", KEYS[1], tonumber(ARGV[1]))
		end
		return count
	`)
	touchApiKeyLastUsedScript = redis.NewScript(`
		local value = redis.call("GET", KEYS[1])
		if not value then return redis.error_reply("api key not found") end
		local api_key = cjson.decode(value)
		api_key.last_used_at = ARGV[1]
		redis.call("SET", KEYS[1], cjson.encode(api_key))
		return 1
	`)
	incrementAccountStatsScript = redis.NewScript(`
		local key = KEYS[1]
		local usage = tonumber(ARGV[1])
		local count = tonumber(ARGV[2])
		local now_str = ARGV[3]
		local today = ARGV[4]
		local operation_id = ARGV[5]
		if operation_id ~= "" then
			if redis.call("SISMEMBER", KEYS[2], operation_id) == 1 then return "DUPLICATE" end
		end
		local val = redis.call("GET", key)
		if not val then return redis.error_reply("account not found") end
		local acc = cjson.decode(val)
		local acc_type = ""
		if acc.account_type ~= nil then
			acc_type = string.lower(tostring(acc.account_type))
		end
		if acc_type ~= "grok" and acc_type ~= "qoder" and acc_type ~= "workbuddy" then
			acc.usage_current = (acc.usage_current or 0) + usage
		end
		acc.usage_total = (acc.usage_total or 0) + usage
		-- The daily figure is rolled here rather than by a scheduled job: a
		-- request observed on a different date than the one recorded starts a
		-- new day. A total that never resets cannot answer how much of the
		-- upstream rate limit this account has spent today.
		if acc.tokens_date == nil or acc.tokens_date < today then
			acc.tokens_date = today
			acc.tokens_today = usage
		elseif acc.tokens_date == today then
			acc.tokens_today = (acc.tokens_today or 0) + usage
		end
		acc.request_count = (acc.request_count or 0) + count
		acc.last_used_at = now_str
		acc.updated_at = now_str
		redis.call("SET", key, cjson.encode(acc))
		if operation_id ~= "" then
			redis.call("SADD", KEYS[2], operation_id)
			redis.call("EXPIRE", KEYS[2], 691200)
		end
		return "OK"
	`)
	// reserveApiKeyBillingScript implements the whole reservation decision in one
	// atomic step: expired holds are pruned, the live holds are summed with the
	// settled counter, and the new hold is only inserted when the key's limit
	// still covers it. A repeat of the same event id with the same amount is
	// idempotent; the same event id with a different amount is a conflict.
	// KEYS: reservations hash, used counter, limit mirror.
	// ARGV: event id, amount ticks, now unix seconds, expiry unix seconds.
	reserveApiKeyBillingScript = redis.NewScript(`
		local event_id = ARGV[1]
		local amount = tonumber(ARGV[2])
		local now = tonumber(ARGV[3])
		local expires_at = tonumber(ARGV[4])
		local limit = tonumber(redis.call("GET", KEYS[3]) or "0") or 0
		local used = tonumber(redis.call("GET", KEYS[2]) or "0") or 0
		local live_sum = 0
		local max_expiry = expires_at
		local fields = redis.call("HGETALL", KEYS[1])
		for i = 1, #fields, 2 do
			local field = fields[i]
			local raw = fields[i + 1]
			local separator = string.find(raw, ":", 1, true)
			local held = nil
			local expiry = nil
			if separator then
				held = tonumber(string.sub(raw, 1, separator - 1))
				expiry = tonumber(string.sub(raw, separator + 1))
			end
			if held == nil or expiry == nil or expiry <= now then
				redis.call("HDEL", KEYS[1], field)
			elseif field == event_id then
				if held == amount then return 1 end
				return redis.error_reply("billing reservation exists with a different amount")
			else
				live_sum = live_sum + held
				if expiry > max_expiry then max_expiry = expiry end
			end
		end
		if limit > 0 and used + live_sum + amount > limit then return 0 end
		redis.call("HSET", KEYS[1], event_id, tostring(amount) .. ":" .. tostring(expires_at))
		redis.call("PEXPIREAT", KEYS[1], max_expiry * 1000 + 60000)
		return 1
	`)
	// settleApiKeyBillingScript books usage exactly once per event id. Recording
	// the event in the same Lua transaction makes a retry after an ambiguous
	// network timeout safe: Redis either applied all three mutations or none.
	settleApiKeyBillingScript = redis.NewScript(`
		local prior = redis.call("HGET", KEYS[3], ARGV[1])
		if prior then
			if prior ~= tostring(ARGV[2]) then return -1 end
			return 0
		end
		redis.call("HSET", KEYS[3], ARGV[1], tostring(ARGV[2]))
		redis.call("HDEL", KEYS[1], ARGV[1])
		redis.call("INCRBY", KEYS[2], tonumber(ARGV[2]))
		return 1
	`)
	releaseApiKeyBillingScript = redis.NewScript(`
		return redis.call("HDEL", KEYS[1], ARGV[1])
	`)
	resetApiKeyBillingScript = redis.NewScript(`
		redis.call("DEL", KEYS[1])
		redis.call("SET", KEYS[2], 0)
		redis.call("DEL", KEYS[3])
		return 1
	`)
	// rolloverApiKeyBillingScript compares and mutates the durable API-key row in
	// one Redis transaction. It deliberately preserves KEYS[2] (live holds), so a
	// request admitted before rollover can still settle or release normally.
	rolloverApiKeyBillingScript = redis.NewScript(`
		local raw = redis.call("GET", KEYS[1])
		if not raw then return redis.error_reply("api key not found") end
		local row = cjson.decode(raw)
		if (tonumber(row.billing_period_days or "0") or 0) ~= tonumber(ARGV[3]) then return 0 end
		local current_start = tostring(row.billing_period_started_at or "")
		if ARGV[1] == "" then
			if current_start ~= "" and string.sub(current_start, 1, 5) ~= "0001-" then return 0 end
		elseif current_start ~= ARGV[1] then
			return 0
		end
		row.billing_period_started_at = ARGV[2]
		row.updated_at = ARGV[2]
		redis.call("SET", KEYS[1], cjson.encode(row))
		redis.call("SET", KEYS[3], 0)
		redis.call("DEL", KEYS[4])
		return 1
	`)
	listModelsScript = redis.NewScript(`
		local ids = redis.call("SMEMBERS", KEYS[1])
		local rows = {}
		for _, id in ipairs(ids) do
			local value = redis.call("GET", ARGV[1] .. id)
			if value then table.insert(rows, value) end
		end
		return rows
	`)
	reconcileDiscoveredModelsScript = redis.NewScript(`
		local row_prefix, channel = ARGV[1], ARGV[2]
		local prune, incoming, provider_scope = ARGV[3] == "1", cjson.decode(ARGV[4]), string.lower(tostring(ARGV[5] or ""))
		local wanted, existing = {}, {}
		local added, updated, deleted, protected = {}, {}, {}, {}
		for _, id in ipairs(redis.call("SMEMBERS", KEYS[1])) do
			local raw = redis.call("GET", row_prefix .. id)
			if raw then
				local row = cjson.decode(raw)
				local row_channel = string.lower(tostring(row.channel or ""))
				row_channel = string.gsub(string.gsub(row_channel, "_", "-"), " ", "-")
				if row_channel == channel and row.model_id then existing[tostring(row.model_id)] = {id=id,row=row} end
			end
		end
		for _, row in ipairs(incoming) do
			local current_incoming = row
			local model_id = tostring(row.model_id)
			wanted[model_id] = true
			local current = existing[model_id]
			if current then
				local origin = string.lower(tostring(current.row.origin or ""))
				if (provider_scope ~= "" and prune) or origin == "discovery" then
					-- An authoritative scoped catalog owns every row in its plane,
					-- including older admin/catalog rows. Non-pruning refreshes retain
					-- the operator-field protection below.
					row.id, row.created_at = current.id, current.row.created_at or row.created_at
				else
					-- Manual/config/legacy rows are never replaced or pruned. A matching
					-- observation may only promote verification and fill metadata that
					-- the operator never supplied.
					row = current.row
					row.verified = true
					if (not row.name or tostring(row.name) == "" or tostring(row.name) == model_id) and current_incoming.name then row.name = current_incoming.name end
					if (not row.provider or tostring(row.provider) == "") and current_incoming.provider then row.provider = current_incoming.provider end
					if (not row.upstream_model or tostring(row.upstream_model) == "") and current_incoming.upstream_model then row.upstream_model = current_incoming.upstream_model end
					if current_incoming.billing_tier ~= nil then row.billing_tier = current_incoming.billing_tier end
					if current_incoming.billing_source ~= nil then row.billing_source = current_incoming.billing_source end
					table.insert(protected, model_id)
				end
				if row.status ~= "available" and row.status ~= "maintenance" and row.status ~= "offline" then row.status = "offline" end
				redis.call("SET", row_prefix .. current.id, cjson.encode(row))
				redis.call("HSET", KEYS[3], channel .. "|" .. model_id, current.id)
				table.insert(updated, model_id)
			else
				local id = tostring(redis.call("INCR", KEYS[2]))
				row.id = id
				redis.call("SET", row_prefix .. id, cjson.encode(row))
				redis.call("SADD", KEYS[1], id)
				redis.call("HSET", KEYS[3], channel .. "|" .. model_id, id)
				table.insert(added, model_id)
			end
		end
		if prune then
			for model_id, current in pairs(existing) do
				local current_provider = string.lower(tostring(current.row.provider or ""))
				if not wanted[model_id] and (provider_scope == "" or current_provider == provider_scope) then
					local origin = string.lower(tostring(current.row.origin or ""))
					if (provider_scope ~= "" and prune) or origin == "discovery" then
						redis.call("DEL", row_prefix .. current.id)
						redis.call("SREM", KEYS[1], current.id)
						local index_key = channel .. "|" .. model_id
						if redis.call("HGET", KEYS[3], index_key) == current.id then redis.call("HDEL", KEYS[3], index_key) end
						table.insert(deleted, model_id)
					elseif origin == "" or origin == "manual" then table.insert(protected, model_id) end
				end
			end
		end
		return cjson.encode({added = added, updated = updated, deleted = deleted, protected = protected})
	`)
)
