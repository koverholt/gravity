/*
Copyright 2018 Gravitational, Inc.

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

package monitoring

import "time"

// Monitoring defines the interface for monitoring provider
type Monitoring interface {
	// GetRetentionPolicies returns a list of retention policies
	GetRetentionPolicies() ([]RetentionPolicy, error)
	// UpdateRetentionPolicy updates a retention policy
	UpdateRetentionPolicy(RetentionPolicy) error
}

// RetentionPolicy represents a single retention policy
type RetentionPolicy struct {
	// Name is the policy name
	Name string `json:"name"`
	// Duration is the policy duration
	Duration time.Duration `json:"duration"`
}
