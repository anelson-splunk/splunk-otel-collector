// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package databricksreceiver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunTracker(t *testing.T) {
	c := &fakeCompletedJobRunClient{}
	tracker := newRunTracker()
	runs, _ := c.completedJobRuns(42, 0)
	latest := tracker.extractNewRuns(runs)
	assert.Equal(t, 0, len(latest))

	runs, _ = c.completedJobRuns(42, 0)
	latest = tracker.extractNewRuns(runs)
	assert.Equal(t, 1, len(latest))

	// simulate no new runs added
	latest = tracker.extractNewRuns(runs)
	assert.Nil(t, latest)

	runs, _ = c.completedJobRuns(42, 0)
	latest = tracker.extractNewRuns(runs)
	assert.Equal(t, 1, len(latest))

	c.addCompletedRun(42)
	runs, _ = c.completedJobRuns(42, 0)
	latest = tracker.extractNewRuns(runs)
	assert.Equal(t, 2, len(latest))
}
