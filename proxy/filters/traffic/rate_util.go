/*
Copyright 2021 The Kruise Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package traffic

import (
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"

	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
)

const (
	defaultRetryDelay = 1
)

type rateControl struct {
	once   sync.Once
	name   string
	policy *ctrlmeshv1alpha1.TrafficRateLimitingPolicy

	maxInFlightChan    chan bool
	bucket             *rate.Limiter
	exponentialBackoff *rateExponentialBackoff
}

func (rc *rateControl) allow(requestInfo *apirequest.RequestInfo) (deferFunc func(int32), delaySeconds int, err error) {
	rc.once.Do(func() {
		if rc.policy.MaxInFlight != nil {
			rc.maxInFlightChan = make(chan bool, *rc.policy.MaxInFlight)
		}
		if rc.policy.Bucket != nil {
			rc.bucket = rate.NewLimiter(rate.Limit(rc.policy.Bucket.QPS), int(rc.policy.Bucket.Burst))
		}
		if rc.policy.ExponentialBackoff != nil {
			rc.exponentialBackoff = &rateExponentialBackoff{TrafficRateLimitingExponentialBackoff: *rc.policy.ExponentialBackoff}
		}
	})

	var maxInFlightDefer, exponentialBackoffDefer func(int32)
	defer func() {
		if maxInFlightDefer != nil || exponentialBackoffDefer != nil {
			deferFunc = func(status int32) {
				if maxInFlightDefer != nil {
					maxInFlightDefer(status)
				}
				if exponentialBackoffDefer != nil {
					exponentialBackoffDefer(status)
				}
			}
		}
	}()

	if rc.maxInFlightChan != nil {
		select {
		case rc.maxInFlightChan <- true:
			maxInFlightDefer = func(int32) {
				<-rc.maxInFlightChan
			}
		default:
			return nil, defaultRetryDelay, fmt.Errorf("rate limiting by TrafficPolicy %s maxInFlight %d", rc.name, *rc.policy.MaxInFlight)
		}
	}
	if rc.bucket != nil {
		delay := rc.bucket.Reserve().Delay()
		if delay > 0 {
			return nil, int(delay/time.Second) + 1, fmt.Errorf("rate limiting by TrafficPolicy %s bucket(%d,%d), delay %v",
				rc.name, rc.policy.Bucket.QPS, rc.policy.Bucket.Burst, delay)
		}
	}
	if rc.exponentialBackoff != nil {
		if delay, ok := rc.exponentialBackoff.delay(*requestInfo); ok {
			exponentialBackoffDefer = func(status int32) {
				if status == 0 {
					return
				}
				// currently considered HTTP code >= 400 as failure
				if status >= http.StatusBadRequest {
					rc.exponentialBackoff.failed(*requestInfo)
				} else {
					rc.exponentialBackoff.succeed(*requestInfo)
				}
			}
			if delay > 0 {
				return nil, int(delay/time.Second) + 1, fmt.Errorf("rate limiting by TrafficPolicy %s exponential backoff, delay %v", rc.name, delay)
			}
		}
	}
	return
}

type rateExponentialBackoff struct {
	failures sync.Map
	ctrlmeshv1alpha1.TrafficRateLimitingExponentialBackoff
}

type rateExponentialFailure struct {
	lock         sync.Mutex
	failureTimes int32
	nextTime     *time.Time
}

func (eb *rateExponentialBackoff) delay(item interface{}) (time.Duration, bool) {
	val, ok := eb.failures.Load(item)
	if !ok {
		return 0, false
	}
	ef := val.(*rateExponentialFailure)
	ef.lock.Lock()
	defer ef.lock.Unlock()
	if ef.nextTime == nil {
		return 0, true
	}
	delay := ef.nextTime.Sub(time.Now())
	if delay < 0 {
		delay = 0
		ef.nextTime = nil
	}
	return delay, true
}

func (eb *rateExponentialBackoff) succeed(item interface{}) {
	eb.failures.Delete(item)
}

func (eb *rateExponentialBackoff) failed(item interface{}) {
	val, _ := eb.failures.LoadOrStore(item, &rateExponentialFailure{})
	ef := val.(*rateExponentialFailure)
	ef.lock.Lock()
	defer ef.lock.Unlock()
	if ef.failureTimes < math.MaxInt32 {
		ef.failureTimes++
	}
	delta := ef.failureTimes - eb.ContinuouslyFailureTimes
	if delta <= 0 {
		return
	}

	// The backoff is capped such that 'calculated' value never overflows.
	backoff := float64(eb.BaseDelayInMillisecond) * math.Pow(2, float64(delta-1))
	maxBackOff := time.Duration(eb.MaxDelayInMillisecond) * time.Millisecond
	if backoff > math.MaxInt64 {
		nextTime := time.Now().Add(maxBackOff)
		ef.nextTime = &nextTime
		return
	}

	calculated := time.Duration(backoff)
	if calculated > maxBackOff {
		nextTime := time.Now().Add(maxBackOff)
		ef.nextTime = &nextTime
		return
	}

	nextTime := time.Now().Add(calculated)
	ef.nextTime = &nextTime
}
