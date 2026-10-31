package ratelimit

import (
	"context"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/sharedstate"
	"github.com/redis/go-redis/v9"
)

// fixedWindow counts a request in KEYS[1] and starts the window (ARGV[1] ms)
// on the first one. INCR and PEXPIRE run atomically in one script, so
// replicas never race on the window start; a key left without a TTL (which a
// crash between the two commands could not produce, but an operator could)
// gets one on the next request.
var fixedWindow = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 or redis.call('PTTL', KEYS[1]) < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return n
`)

// redisTimeout bounds one limiter call; past it the request is decided by
// the in-process limiter.
const redisTimeout = 500 * time.Millisecond

// sharedWindow is the Redis-backed fixed window behind an IPRateLimiter.
type sharedWindow struct {
	client   *redis.Client
	prefix   string
	fallback *sharedstate.Fallback
}

// UseRedis makes the limiter share its per-IP windows through Redis under
// "ae:rl:ip:<scope>:<ip>", so every replica that uses the same scope draws on
// one budget. A nil client (REDIS_URL unset) leaves the limiter in-process.
// When Redis is unreachable the request is decided by the in-process
// limiter, the outage is logged once, and ae_redis_fallback_total
// {component="ratelimit:<scope>"} counts each such request. It returns rl so
// it can be chained after New.
func (rl *IPRateLimiter) UseRedis(c *redis.Client, scope string) *IPRateLimiter {
	if c == nil {
		return rl
	}
	rl.shared = &sharedWindow{
		client:   c,
		prefix:   "ae:rl:ip:" + scope + ":",
		fallback: sharedstate.NewFallback("ratelimit:"+scope, 0),
	}
	return rl
}

// allow returns (allowed, true) when Redis decided, or (false, false) when
// the caller must decide locally.
func (s *sharedWindow) allow(ip string, max int, window time.Duration) (bool, bool) {
	if !s.fallback.Available() {
		s.fallback.Skipped()
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisTimeout)
	defer cancel()
	n, err := fixedWindow.Run(ctx, s.client, []string{s.prefix + ip}, window.Milliseconds()).Int64()
	if err != nil {
		s.fallback.Fail(err)
		return false, false
	}
	s.fallback.Recover()
	return n <= int64(max), true
}
