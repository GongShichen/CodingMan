package agent

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"net"
	"reflect"
	"strings"
	"time"
)

const (
	defaultRetryInitialDelay = time.Second
	defaultRetryMaxDelay     = 60 * time.Second
	defaultMaxRetries        = 3
	defaultRetryMultiplier   = 2.0
	defaultRetryJitter       = 0.2
)

type RetryConfig struct {
	MaxRetries   int
	InitialDelay time.Duration
	MaxDelay     time.Duration
	Multiplier   float64
	Jitter       float64
	Retryable    func(error) bool
}

func defaultRetryConfig(config RetryConfig) RetryConfig {
	if config.MaxRetries <= 0 {
		config.MaxRetries = defaultMaxRetries
	}
	if config.InitialDelay <= 0 {
		config.InitialDelay = defaultRetryInitialDelay
	}
	if config.MaxDelay <= 0 {
		config.MaxDelay = defaultRetryMaxDelay
	}
	if config.Multiplier <= 1 {
		config.Multiplier = defaultRetryMultiplier
	}
	if config.Jitter < 0 {
		config.Jitter = 0
	}
	if config.Jitter > 1 {
		config.Jitter = 1
	}
	if config.Jitter == 0 {
		config.Jitter = defaultRetryJitter
	}
	if config.Retryable == nil {
		config.Retryable = IsRetryableLLMError
	}
	if config.InitialDelay > config.MaxDelay {
		config.InitialDelay = config.MaxDelay
	}
	return config
}

func retryChat(ctx context.Context, llm LLM, messages []Message, opts ChatOptions, config RetryConfig) (LLMResponse, error) {
	config = defaultRetryConfig(config)
	delay := config.InitialDelay

	var lastResp LLMResponse
	var lastErr error
	for attempt := 0; attempt <= config.MaxRetries; attempt++ {
		resp, err := llm.Chat(ctx, messages, opts)
		resp.RetryAttempts = attempt
		if err == nil {
			return resp, nil
		}

		lastResp = resp
		lastErr = err
		if attempt == config.MaxRetries || !config.Retryable(err) {
			break
		}

		if err := sleepWithContext(ctx, jitterDelay(delay, config.Jitter)); err != nil {
			return LLMResponse{StopReason: err.Error()}, err
		}
		delay = time.Duration(math.Round(float64(delay) * config.Multiplier))
		if delay > config.MaxDelay {
			delay = config.MaxDelay
		}
	}

	return lastResp, lastErr
}

func IsRetryableLLMError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}

	statusCode, ok := errorStatusCode(err)
	if ok {
		return statusCode == 408 ||
			statusCode == 409 ||
			statusCode == 429 ||
			statusCode >= 500
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}

	text := strings.ToLower(err.Error())
	if strings.Contains(text, "context deadline exceeded") ||
		strings.Contains(text, "timeout") ||
		strings.Contains(text, "temporar") ||
		strings.Contains(text, "connection reset") ||
		strings.Contains(text, "connection refused") ||
		strings.Contains(text, "eof") ||
		strings.Contains(text, "rate limit") ||
		strings.Contains(text, "too many requests") ||
		strings.Contains(text, "overloaded") ||
		strings.Contains(text, "server error") {
		return true
	}
	if strings.Contains(text, "401") ||
		strings.Contains(text, "403") ||
		strings.Contains(text, "400 bad request") ||
		strings.Contains(text, "invalid request") ||
		strings.Contains(text, "unsupported") {
		return false
	}

	return false
}

func errorStatusCode(err error) (int, bool) {
	for err != nil {
		if withStatus, ok := err.(interface{ StatusCode() int }); ok {
			return withStatus.StatusCode(), true
		}

		value := reflect.ValueOf(err)
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				return 0, false
			}
			value = value.Elem()
		}
		if value.IsValid() && value.Kind() == reflect.Struct {
			field := value.FieldByName("StatusCode")
			if field.IsValid() && field.CanInt() {
				return int(field.Int()), true
			}
			if field.IsValid() && field.CanUint() {
				return int(field.Uint()), true
			}
		}

		err = errors.Unwrap(err)
	}
	return 0, false
}

func jitterDelay(delay time.Duration, jitter float64) time.Duration {
	if jitter <= 0 {
		return delay
	}
	delta := (rand.Float64()*2 - 1) * jitter
	result := time.Duration(float64(delay) * (1 + delta))
	if result < 0 {
		return 0
	}
	return result
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
