package router

import (
	"context"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/sharedstate"
	"github.com/redis/go-redis/v9"
)

// limiter is what the Router needs from a rate limiter. *RateLimiter (one
// process) and *SharedRateLimiter (Redis, shared by replicas) implement it.
type limiter interface {
	Allow() bool
	WaitTime() time.Duration
}

// tokenBucket refills KEYS[1] from the Redis server clock and, when ARGV[3]
// is 1, takes one token. It returns {allowed, wait_ms}. Using the server
// clock keeps replicas with skewed clocks on one timeline. The bucket key
// expires once it would be full again, so idle buckets cost nothing.
//
//	ARGV[1] capacity, ARGV[2] refill in tokens per millisecond, ARGV[3] take (0|1)
var tokenBucket = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local max = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local take = tonumber(ARGV[3])
local b = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(b[1])
local ts = tonumber(b[2])
if tokens == nil or ts == nil then
  tokens = max
  ts = now
end
if now > ts then
  tokens = math.min(max, tokens + (now - ts) * rate)
  ts = now
end
local allowed = 0
local wait = 0
if tokens >= 1 then
  allowed = 1
  if take == 1 then tokens = tokens - 1 end
else
  wait = math.ceil((1 - tokens) / rate)
end
redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', tostring(ts))
redis.call('PEXPIRE', KEYS[1], math.ceil(max / rate) + 1000)
return {allowed, wait}
`)

// sharedTimeout bounds one Redis call; past it the local bucket decides.
const sharedTimeout = 500 * time.Millisecond

// SharedRateLimiter is a token bucket kept in Redis under "ae:rl:llm:<key>",
// so every replica that uses the same key draws on one bucket. When Redis is
// unreachable the in-process bucket decides, the outage is logged once, and
// ae_redis_fallback_total{component="llm-router"} counts each such call.
type SharedRateLimiter struct {
	client     *redis.Client
	key        string
	maxTokens  int
	refillPerS float64 // tokens per second
	local      *RateLimiter
	fallback   *sharedstate.Fallback
}

// NewSharedRateLimiter returns a bucket with the same capacity and refill as
// NewRateLimiter(maxTokens, refillPerMinute), stored in Redis.
func NewSharedRateLimiter(c *redis.Client, key string, maxTokens int, refillPerMinute float64) *SharedRateLimiter {
	return &SharedRateLimiter{
		client:     c,
		key:        "ae:rl:llm:" + key,
		maxTokens:  maxTokens,
		refillPerS: refillPerMinute / 60.0,
		local:      NewRateLimiter(maxTokens, refillPerMinute),
		// One tracker per process: routers are built per message.
		fallback: sharedstate.ProcessFallback("llm-router"),
	}
}

// run executes the script; ok is false when the caller must use the local
// bucket.
func (s *SharedRateLimiter) run(take int) (allowed bool, wait time.Duration, ok bool) {
	if !s.fallback.Available() {
		s.fallback.Skipped()
		return false, 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
	defer cancel()
	res, err := tokenBucket.Run(ctx, s.client, []string{s.key},
		s.maxTokens, s.refillPerS/1000.0, take).Int64Slice()
	if err != nil || len(res) != 2 {
		if err == nil {
			err = redis.Nil
		}
		s.fallback.Fail(err)
		return false, 0, false
	}
	s.fallback.Recover()
	return res[0] == 1, time.Duration(res[1]) * time.Millisecond, true
}

// Allow takes one token and reports whether the call may proceed.
func (s *SharedRateLimiter) Allow() bool {
	if allowed, _, ok := s.run(1); ok {
		return allowed
	}
	return s.local.Allow()
}

// WaitTime returns how long until a token is available, without taking it.
func (s *SharedRateLimiter) WaitTime() time.Duration {
	if _, wait, ok := s.run(0); ok {
		return wait
	}
	return s.local.WaitTime()
}
