package main

import (
	"sync"
	"time"
)

// RateLimiter controls the rate of tile downloads
type RateLimiter struct {
	tokens chan struct{}
	delay  time.Duration
	mu     sync.Mutex
}

var tileRateLimiter *RateLimiter

func init() {
	tileRateLimiter = NewRateLimiter(200, 10*time.Millisecond)
}

// NewRateLimiter creates a rate limiter with maxConcurrent tokens
func NewRateLimiter(maxConcurrent int, delay time.Duration) *RateLimiter {
	rl := &RateLimiter{
		tokens: make(chan struct{}, maxConcurrent),
		delay:  delay,
	}
	for i := 0; i < maxConcurrent; i++ {
		rl.tokens <- struct{}{}
	}
	return rl
}

// Acquire gets a token, blocking if none available
func (rl *RateLimiter) Acquire() {
	<-rl.tokens
	rl.mu.Lock()
	time.Sleep(rl.delay)
	rl.mu.Unlock()
}

// Release returns a token to the pool
func (rl *RateLimiter) Release() {
	rl.tokens <- struct{}{}
}
